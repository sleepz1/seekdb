/* seekdb <-> hipVS bridge: plain-C CAGRA kNN; all cuVS/ROCm deps live here. */
#include "seekdb_cuvs_bridge.h"
#include <cuvs/core/c_api.h>
#include <cuvs/neighbors/cagra.h>
#include <dlpack/dlpack.h>
#ifdef __HIP_PLATFORM_AMD__
#include <cuvs/cuda_runtime.h>
#else
#include <cuda_runtime.h>
#endif
#include <hip/hip_runtime_api.h>
#include <stdio.h>
#include <stdlib.h>
#include <stdint.h>
#include <string.h>
#include <time.h>

typedef struct {
  cuvsResources_t        res;
  cuvsCagraIndexParams_t ip;
  cuvsCagraIndex_t       idx;
  long n, dim;
  void* d_dataset;  /* device buffers backing an index built from a custom graph */
  void* d_graph;
} sk_cuvs_index;

/* Custom GPU graph builder (seekdb_cuvs_graph.hip): exact GPU kNN + CAGRA pruning. */
int sk_build_cagra_graph(const float* d_x, long n, int dim, int K, int D,
                         uint32_t* d_graph, hipStream_t st, double* t_knn, double* t_prune);

#define SK_GRAPH_DEGREE 128
#define SK_INTERMEDIATE_DEGREE 256
#define SK_CUSTOM_MIN_ROWS 2048
#define SK_CUSTOM_MAX_ROWS (8L << 20)

static double sk_now(void)
{
  struct timespec t; clock_gettime(CLOCK_MONOTONIC, &t);
  return t.tv_sec + t.tv_nsec * 1e-9;
}

static void sk_tensor(DLManagedTensor* t, void* data, int on_device, long* shape, int code, int bits)
{
  memset(t, 0, sizeof(*t));
  t->dl_tensor.data = data;
  t->dl_tensor.device.device_type = on_device ? kDLCUDA : kDLCPU;
  t->dl_tensor.device.device_id = 0;
  t->dl_tensor.ndim = 2;
  t->dl_tensor.dtype.code = code; t->dl_tensor.dtype.bits = bits; t->dl_tensor.dtype.lanes = 1;
  t->dl_tensor.shape = shape;
}

/* Build the CAGRA index from a graph produced on the GPU by sk_build_cagra_graph.
 * Returns 0 on success; on failure everything allocated here is released. */
static int sk_build_custom(sk_cuvs_index* h, const float* base, long n, long dim)
{
  if (n < SK_CUSTOM_MIN_ROWS || n > SK_CUSTOM_MAX_ROWS || dim <= 0 || dim > 65536) return 1;
  if (getenv("SEEKDB_CUVS_NND_BUILD") != NULL) return 1;
  const double t0 = sk_now();
  hipStream_t st = 0;
  void* dx = NULL; void* dg = NULL;
  double t_knn = 0, t_prune = 0;
  int rc = 0;
  if (hipStreamCreateWithFlags(&st, hipStreamNonBlocking) != hipSuccess) return 2;
  if (hipMalloc(&dx, sizeof(float) * (size_t)n * dim) != hipSuccess ||
      hipMalloc(&dg, sizeof(uint32_t) * (size_t)n * SK_GRAPH_DEGREE) != hipSuccess) { rc = 3; goto fail; }
  if (hipMemcpyAsync(dx, base, sizeof(float) * (size_t)n * dim, hipMemcpyHostToDevice, st) != hipSuccess) { rc = 4; goto fail; }
  rc = sk_build_cagra_graph((const float*)dx, n, (int)dim, SK_INTERMEDIATE_DEGREE, SK_GRAPH_DEGREE,
                            (uint32_t*)dg, st, &t_knn, &t_prune);
  if (rc != 0) { rc = 10 + rc; goto fail; }
  if (cuvsCagraIndexCreate(&h->idx) != CUVS_SUCCESS) { rc = 5; goto fail; }
  {
    long gs[2] = {n, SK_GRAPH_DEGREE}, ds[2] = {n, dim};
    DLManagedTensor gt, dt;
    sk_tensor(&gt, dg, 1, gs, kDLUInt, 32);
    sk_tensor(&dt, dx, 1, ds, kDLFloat, 32);
    if (cuvsCagraIndexFromArgs(h->res, L2Expanded, &gt, &dt, h->idx) != CUVS_SUCCESS ||
        cuvsStreamSync(h->res) != CUVS_SUCCESS) {
      cuvsCagraIndexDestroy(h->idx); h->idx = 0; rc = 6; goto fail;
    }
  }
  hipStreamDestroy(st);
  h->d_dataset = dx; h->d_graph = dg;
  fprintf(stderr, "[seekdb_cuvs] gpu graph build n=%ld dim=%ld knn=%.3fs prune=%.3fs total=%.3fs\n",
          n, dim, t_knn, t_prune, sk_now() - t0);
  return 0;
fail:
  if (st) { hipStreamSynchronize(st); hipStreamDestroy(st); }
  if (dx) hipFree(dx);
  if (dg) hipFree(dg);
  fprintf(stderr, "[seekdb_cuvs] gpu graph build failed rc=%d, falling back to NN-Descent: %s\n",
          rc, cuvsGetLastErrorText());
  return rc;
}

void* seekdb_cuvs_build(const float* base, long n, long dim)
{
  sk_cuvs_index* h = (sk_cuvs_index*)calloc(1, sizeof(*h));
  if (!h) return NULL;
  if (cuvsResourcesCreate(&h->res) != CUVS_SUCCESS) { free(h); return NULL; }
  if (cuvsCagraIndexParamsCreate(&h->ip) != CUVS_SUCCESS) { cuvsResourcesDestroy(h->res); free(h); return NULL; }
  if (sk_build_custom(h, base, n, dim) == 0) { h->n = n; h->dim = dim; return h; }
  cuvsCagraIndexParamsDestroy(h->ip); h->ip = 0;

  DLManagedTensor dset; long ds[2] = {n, dim};
  dset.dl_tensor.data = (void*)base;
  dset.dl_tensor.device.device_type = kDLCPU; dset.dl_tensor.device.device_id = 0;
  dset.dl_tensor.ndim = 2;
  dset.dl_tensor.dtype.code = kDLFloat; dset.dl_tensor.dtype.bits = 32; dset.dl_tensor.dtype.lanes = 1;
  dset.dl_tensor.shape = ds; dset.dl_tensor.strides = 0; dset.dl_tensor.byte_offset = 0;

  if (cuvsCagraIndexParamsCreate(&h->ip) != CUVS_SUCCESS) goto fail_res;
  h->ip->build_algo = NN_DESCENT;
  h->ip->graph_degree = 128;
  h->ip->intermediate_graph_degree = 256;
  h->ip->nn_descent_niter = 100;
  if (cuvsCagraIndexCreate(&h->idx)      != CUVS_SUCCESS) goto fail_ip;
  if (cuvsCagraBuild(h->res, h->ip, &dset, h->idx) != CUVS_SUCCESS) goto fail_idx;
  h->n = n; h->dim = dim;
  return h;

fail_idx: cuvsCagraIndexDestroy(h->idx);
fail_ip:  cuvsCagraIndexParamsDestroy(h->ip);
fail_res: cuvsResourcesDestroy(h->res); free(h);
  fprintf(stderr, "[seekdb_cuvs] build failed: %s\n", cuvsGetLastErrorText());
  return NULL;
}

int seekdb_cuvs_search(void* handle, const float* query, long nq, long topk,
                       unsigned int* out_ids, float* out_dist)
{
  sk_cuvs_index* h = (sk_cuvs_index*)handle;
  if (!h) return 1;
  int rc = 0;
  float* qd = 0; unsigned* nd = 0; float* dd = 0; cuvsCagraSearchParams_t sp = 0;
  if (cuvsRMMAlloc(h->res, (void**)&qd, sizeof(float)*nq*h->dim)  != CUVS_SUCCESS) { rc=10; goto done; }
  if (cuvsRMMAlloc(h->res, (void**)&nd, sizeof(unsigned)*nq*topk) != CUVS_SUCCESS) { rc=11; goto done; }
  if (cuvsRMMAlloc(h->res, (void**)&dd, sizeof(float)*nq*topk)    != CUVS_SUCCESS) { rc=12; goto done; }
  if (cudaMemcpy(qd, query, sizeof(float)*nq*h->dim, cudaMemcpyDefault) != cudaSuccess) { rc=13; goto done; }
  {
    DLManagedTensor qt, nt, dt; long qs[2]={nq,h->dim}, nsh[2]={nq,topk}, dsh[2]={nq,topk};
    qt.dl_tensor.data=qd; qt.dl_tensor.device.device_type=kDLCUDA; qt.dl_tensor.device.device_id=0;
    qt.dl_tensor.ndim=2; qt.dl_tensor.dtype.code=kDLFloat; qt.dl_tensor.dtype.bits=32; qt.dl_tensor.dtype.lanes=1;
    qt.dl_tensor.shape=qs; qt.dl_tensor.strides=0; qt.dl_tensor.byte_offset=0;
    nt=qt; nt.dl_tensor.data=nd; nt.dl_tensor.dtype.code=kDLUInt; nt.dl_tensor.shape=nsh;
    dt=qt; dt.dl_tensor.data=dd; dt.dl_tensor.dtype.code=kDLFloat; dt.dl_tensor.shape=dsh;
    if (cuvsCagraSearchParamsCreate(&sp) != CUVS_SUCCESS) { rc=14; goto done; }
    {
      size_t required_itopk = (size_t)topk * 4;
      if (required_itopk < 512) required_itopk = 512;
      if (sp->itopk_size < required_itopk) sp->itopk_size = required_itopk;
      sp->search_width = 4;
    }
    cuvsFilter f; f.type=NO_FILTER; f.addr=(uintptr_t)0;
    if (cuvsCagraSearch(h->res, sp, h->idx, &qt, &nt, &dt, f) != CUVS_SUCCESS) { rc=15; goto done; }
    if (cudaMemcpy(out_ids, nd, sizeof(unsigned)*nq*topk, cudaMemcpyDefault) != cudaSuccess) { rc=16; goto done; }
    if (out_dist) cudaMemcpy(out_dist, dd, sizeof(float)*nq*topk, cudaMemcpyDefault);
  }
done:
  if (sp) cuvsCagraSearchParamsDestroy(sp);
  if (dd) cuvsRMMFree(h->res, dd, sizeof(float)*nq*topk);
  if (nd) cuvsRMMFree(h->res, nd, sizeof(unsigned)*nq*topk);
  if (qd) cuvsRMMFree(h->res, qd, sizeof(float)*nq*h->dim);
  if (rc) fprintf(stderr, "[seekdb_cuvs] search rc=%d: %s\n", rc, cuvsGetLastErrorText());
  return rc;
}

void seekdb_cuvs_free(void* handle)
{
  sk_cuvs_index* h = (sk_cuvs_index*)handle;
  if (!h) return;
  cuvsCagraIndexDestroy(h->idx);
  cuvsCagraIndexParamsDestroy(h->ip);
  if (h->d_graph) hipFree(h->d_graph);
  if (h->d_dataset) hipFree(h->d_dataset);
  cuvsResourcesDestroy(h->res);
  free(h);
}

int seekdb_cuvs_serialize(void* handle, const char* filename)
{
  sk_cuvs_index* index = (sk_cuvs_index*)handle;
  if (!index || !filename) return 1;
  if (cuvsCagraSerialize(index->res, filename, index->idx, true) != CUVS_SUCCESS) {
    fprintf(stderr, "[seekdb_cuvs] serialize failed: %s\n", cuvsGetLastErrorText());
    return 1;
  }
  return 0;
}

void* seekdb_cuvs_deserialize(const char* filename, long n, long dim)
{
  if (!filename || n <= 0 || dim <= 0) return NULL;
  sk_cuvs_index* index = (sk_cuvs_index*)calloc(1, sizeof(*index));
  if (!index) return NULL;
  if (cuvsResourcesCreate(&index->res) != CUVS_SUCCESS) goto fail;
  if (cuvsCagraIndexParamsCreate(&index->ip) != CUVS_SUCCESS) goto fail_res;
  if (cuvsCagraIndexCreate(&index->idx) != CUVS_SUCCESS) goto fail_ip;
  if (cuvsCagraDeserialize(index->res, filename, index->idx) != CUVS_SUCCESS) goto fail_idx;
  {
    int64_t actual_n = 0;
    int64_t actual_dim = 0;
    if (cuvsCagraIndexGetSize(index->idx, &actual_n) != CUVS_SUCCESS ||
        cuvsCagraIndexGetDims(index->idx, &actual_dim) != CUVS_SUCCESS ||
        actual_n != n || actual_dim != dim) {
      fprintf(stderr, "[seekdb_cuvs] snapshot shape mismatch: %ld x %ld, expected %ld x %ld\n",
              (long)actual_n, (long)actual_dim, n, dim);
      goto fail_idx;
    }
  }
  index->n = n;
  index->dim = dim;
  return index;
fail_idx:
  cuvsCagraIndexDestroy(index->idx);
fail_ip:
  cuvsCagraIndexParamsDestroy(index->ip);
fail_res:
  cuvsResourcesDestroy(index->res);
fail:
  fprintf(stderr, "[seekdb_cuvs] deserialize failed: %s\n", cuvsGetLastErrorText());
  free(index);
  return NULL;
}

int seekdb_cuvs_cagra_knn(const float* base, long n, long dim,
                          const float* query, long nq, long topk,
                          unsigned int* out_ids)
{
  void* h = seekdb_cuvs_build(base, n, dim);
  if (!h) return 10;
  int rc = seekdb_cuvs_search(h, query, nq, topk, out_ids, NULL);
  seekdb_cuvs_free(h);
  return rc;
}
