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
#include <pthread.h>

typedef struct {
  cuvsResources_t        res;
  cuvsCagraIndexParams_t ip;
  cuvsCagraIndex_t       idx;
  long n, dim;
  void* d_dataset;  /* device buffers backing an index built from a custom graph */
  void* d_graph;
  /* Persistent small-batch search state (guarded by mu): avoids per-query
   * allocator round trips and parameter construction on the hot path. */
  pthread_mutex_t mu;
  void* q_buf; void* n_buf; void* d_buf;
  size_t q_cap, r_cap;   /* capacities in elements */
  cuvsCagraSearchParams_t sp_small;
} sk_cuvs_index;

/* Custom GPU graph builder (seekdb_cuvs_graph.hip): exact GPU kNN + CAGRA pruning. */
int sk_build_cagra_graph(float** pd_x, const float* h_x, long n, int dim, int K, int D,
                         uint32_t** pd_graph, hipStream_t st, double* t_knn, double* t_prune);

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
  if (hipMalloc(&dx, sizeof(float) * (size_t)n * dim) != hipSuccess) { rc = 3; goto fail; }
  if (hipMemcpyAsync(dx, base, sizeof(float) * (size_t)n * dim, hipMemcpyHostToDevice, st) != hipSuccess) { rc = 4; goto fail; }
  rc = sk_build_cagra_graph((float**)&dx, base, n, (int)dim, SK_INTERMEDIATE_DEGREE, SK_GRAPH_DEGREE,
                            (uint32_t**)&dg, st, &t_knn, &t_prune);
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
  if (h) pthread_mutex_init(&h->mu, NULL);
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

/* Original search path: per-call buffers, SINGLE_CTA, itopk>=max(512,4k), width 4. */
static int sk_search_generic(sk_cuvs_index* h, const float* query, long nq, long topk,
                             unsigned int* out_ids, float* out_dist)
{
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

/* Low-latency path for small batches (single-row SQL queries): MULTI_CTA spreads one
 * query over many CTAs instead of one, with a larger internal top-k (>= the generic
 * path's) so accuracy does not regress; device buffers/params persist in the handle. */
#define SK_SMALL_BATCH 16
#define SK_SMALL_MAX_TOPK 256
static int sk_search_small(sk_cuvs_index* h, const float* query, long nq, long topk,
                           unsigned int* out_ids, float* out_dist)
{
  const size_t qn = (size_t)nq * h->dim, rn = (size_t)nq * topk;
  if (qn > h->q_cap) {
    if (h->q_buf) { hipFree(h->q_buf); h->q_buf = 0; h->q_cap = 0; }
    size_t cap = (size_t)SK_SMALL_BATCH * h->dim;
    if (hipMalloc(&h->q_buf, sizeof(float) * cap) != hipSuccess) { h->q_buf = 0; return 1; }
    h->q_cap = cap;
  }
  if (rn > h->r_cap) {
    if (h->n_buf) { hipFree(h->n_buf); h->n_buf = 0; }
    if (h->d_buf) { hipFree(h->d_buf); h->d_buf = 0; }
    h->r_cap = 0;
    size_t cap = (size_t)SK_SMALL_BATCH * SK_SMALL_MAX_TOPK;
    if (hipMalloc(&h->n_buf, sizeof(unsigned) * cap) != hipSuccess) { h->n_buf = 0; return 1; }
    if (hipMalloc(&h->d_buf, sizeof(float) * cap) != hipSuccess) { h->d_buf = 0; return 1; }
    h->r_cap = cap;
  }
  if (!h->sp_small) {
    if (cuvsCagraSearchParamsCreate(&h->sp_small) != CUVS_SUCCESS) { h->sp_small = 0; return 1; }
    h->sp_small->algo = MULTI_CTA;
    h->sp_small->search_width = 4;
  }
  size_t itopk = (size_t)topk * 8;
  if (itopk < 1024) itopk = 1024;
  h->sp_small->itopk_size = itopk;
  cudaStream_t st = 0;
  if (cuvsStreamGet(h->res, &st) != CUVS_SUCCESS) return 1;
  if (hipMemcpyAsync(h->q_buf, query, sizeof(float) * qn, hipMemcpyHostToDevice, (hipStream_t)st) != hipSuccess) return 1;
  DLManagedTensor qt, nt, dt; long qs[2] = {nq, h->dim}, rs[2] = {nq, topk};
  sk_tensor(&qt, h->q_buf, 1, qs, kDLFloat, 32);
  sk_tensor(&nt, h->n_buf, 1, rs, kDLUInt, 32);
  sk_tensor(&dt, h->d_buf, 1, rs, kDLFloat, 32);
  cuvsFilter f; f.type = NO_FILTER; f.addr = (uintptr_t)0;
  if (cuvsCagraSearch(h->res, h->sp_small, h->idx, &qt, &nt, &dt, f) != CUVS_SUCCESS) return 1;
  if (hipMemcpyAsync(out_ids, h->n_buf, sizeof(unsigned) * rn, hipMemcpyDeviceToHost, (hipStream_t)st) != hipSuccess) return 1;
  if (out_dist && hipMemcpyAsync(out_dist, h->d_buf, sizeof(float) * rn, hipMemcpyDeviceToHost, (hipStream_t)st) != hipSuccess) return 1;
  if (hipStreamSynchronize((hipStream_t)st) != hipSuccess) return 1;
  return 0;
}

static int sk_search_impl(sk_cuvs_index* h, const float* query, long nq, long topk,
                          unsigned int* out_ids, float* out_dist);

/* seekdb calls into the bridge from a freshly created pthread for every query, and
 * per-thread HIP/hipVS state set-up dominates the latency of a single-row search.
 * Searches are therefore handed to one long-lived worker thread that keeps that
 * state warm; if the worker cannot be started the search runs inline. */
typedef struct {
  pthread_mutex_t mu; pthread_cond_t cv_job, cv_done; pthread_mutex_t submit;
  int has_job, done, ok;
  sk_cuvs_index* h; const float* query; long nq, topk; unsigned int* out_ids; float* out_dist; int rc;
} sk_search_worker;
static sk_search_worker g_sw = { PTHREAD_MUTEX_INITIALIZER, PTHREAD_COND_INITIALIZER,
                                 PTHREAD_COND_INITIALIZER, PTHREAD_MUTEX_INITIALIZER,
                                 0, 0, 0, 0, 0, 0, 0, 0, 0, 0 };
static pthread_once_t g_sw_once = PTHREAD_ONCE_INIT;

static void* sk_worker_main(void* arg)
{
  (void)arg;
  pthread_mutex_lock(&g_sw.mu);
  for (;;) {
    while (!g_sw.has_job) pthread_cond_wait(&g_sw.cv_job, &g_sw.mu);
    g_sw.has_job = 0;
    pthread_mutex_unlock(&g_sw.mu);
    int rc = sk_search_impl(g_sw.h, g_sw.query, g_sw.nq, g_sw.topk, g_sw.out_ids, g_sw.out_dist);
    pthread_mutex_lock(&g_sw.mu);
    g_sw.rc = rc; g_sw.done = 1;
    pthread_cond_signal(&g_sw.cv_done);
  }
  return NULL;
}

static void sk_worker_start(void)
{
  if (getenv("SEEKDB_CUVS_INLINE_SEARCH") != NULL) return;
  pthread_attr_t attr; pthread_t tid;
  if (pthread_attr_init(&attr) != 0) return;
  pthread_attr_setstacksize(&attr, 64UL * 1024 * 1024);
  pthread_attr_setdetachstate(&attr, PTHREAD_CREATE_DETACHED);
  if (pthread_create(&tid, &attr, sk_worker_main, NULL) == 0) g_sw.ok = 1;
  pthread_attr_destroy(&attr);
}

int seekdb_cuvs_search(void* handle, const float* query, long nq, long topk,
                       unsigned int* out_ids, float* out_dist)
{
  sk_cuvs_index* h = (sk_cuvs_index*)handle;
  if (!h) return 1;
  pthread_once(&g_sw_once, sk_worker_start);
  if (!g_sw.ok) return sk_search_impl(h, query, nq, topk, out_ids, out_dist);
  pthread_mutex_lock(&g_sw.submit);
  pthread_mutex_lock(&g_sw.mu);
  g_sw.h = h; g_sw.query = query; g_sw.nq = nq; g_sw.topk = topk;
  g_sw.out_ids = out_ids; g_sw.out_dist = out_dist; g_sw.done = 0; g_sw.has_job = 1;
  pthread_cond_signal(&g_sw.cv_job);
  while (!g_sw.done) pthread_cond_wait(&g_sw.cv_done, &g_sw.mu);
  int rc = g_sw.rc;
  pthread_mutex_unlock(&g_sw.mu);
  pthread_mutex_unlock(&g_sw.submit);
  return rc;
}

static int sk_search_impl(sk_cuvs_index* h, const float* query, long nq, long topk,
                          unsigned int* out_ids, float* out_dist)
{
  if (nq > 0 && nq <= SK_SMALL_BATCH && topk > 0 && topk <= SK_SMALL_MAX_TOPK &&
      h->n > (long)topk * 8 && getenv("SEEKDB_CUVS_GENERIC_SEARCH") == NULL &&
      pthread_mutex_lock(&h->mu) == 0) {
    int rc = sk_search_small(h, query, nq, topk, out_ids, out_dist);
    pthread_mutex_unlock(&h->mu);
    if (rc == 0) return 0;
    (void)hipGetLastError();
    fprintf(stderr, "[seekdb_cuvs] small-batch search failed, using generic path: %s\n",
            cuvsGetLastErrorText());
  }
  return sk_search_generic(h, query, nq, topk, out_ids, out_dist);
}

static void sk_release_search_state(sk_cuvs_index* h)
{
  if (h->sp_small) cuvsCagraSearchParamsDestroy(h->sp_small);
  if (h->q_buf) hipFree(h->q_buf);
  if (h->n_buf) hipFree(h->n_buf);
  if (h->d_buf) hipFree(h->d_buf);
  h->sp_small = 0; h->q_buf = h->n_buf = h->d_buf = 0; h->q_cap = h->r_cap = 0;
  pthread_mutex_destroy(&h->mu);
}

void seekdb_cuvs_free(void* handle)
{
  sk_cuvs_index* h = (sk_cuvs_index*)handle;
  if (!h) return;
  sk_release_search_state(h);
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
  pthread_mutex_init(&index->mu, NULL);
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

/* Compact snapshot support: the caller persists ids + vectors itself, so the
 * snapshot only needs the CAGRA adjacency list (n x degree uint32). */
long seekdb_cuvs_graph_degree(void* handle)
{
  sk_cuvs_index* h = (sk_cuvs_index*)handle;
  int64_t deg = 0;
  if (!h || !h->idx || cuvsCagraIndexGetGraphDegree(h->idx, &deg) != CUVS_SUCCESS) return -1;
  return (long)deg;
}

int seekdb_cuvs_export_graph(void* handle, unsigned int* out, long n, long degree)
{
  sk_cuvs_index* h = (sk_cuvs_index*)handle;
  if (!h || !out || n != h->n || degree != seekdb_cuvs_graph_degree(handle)) return 1;
  DLManagedTensor g;
  memset(&g, 0, sizeof(g));
  if (cuvsCagraIndexGetGraph(h->idx, &g) != CUVS_SUCCESS || g.dl_tensor.data == NULL) return 2;
  const size_t bytes = sizeof(uint32_t) * (size_t)n * (size_t)degree;
  const char* src = (const char*)g.dl_tensor.data + g.dl_tensor.byte_offset;
  int rc = 0;
  if (g.dl_tensor.ndim != 2 || g.dl_tensor.shape[0] != n || g.dl_tensor.shape[1] != degree ||
      g.dl_tensor.dtype.bits != 32) {
    rc = 3;
  } else if (g.dl_tensor.device.device_type == kDLCPU) {
    memcpy(out, src, bytes);
  } else if (hipMemcpy(out, src, bytes, hipMemcpyDeviceToHost) != hipSuccess) {
    rc = 4;
  }
  if (g.deleter) g.deleter(&g);
  return rc;
}

void* seekdb_cuvs_from_graph(const float* base, const unsigned int* graph, long n, long dim, long degree)
{
  if (!base || !graph || n <= 0 || dim <= 0 || degree <= 0 || degree >= n) return NULL;
  sk_cuvs_index* h = (sk_cuvs_index*)calloc(1, sizeof(*h));
  if (!h) return NULL;
  pthread_mutex_init(&h->mu, NULL);
  void* dx = NULL; void* dg = NULL;
  const size_t xb = sizeof(float) * (size_t)n * dim, gb = sizeof(uint32_t) * (size_t)n * degree;
  if (cuvsResourcesCreate(&h->res) != CUVS_SUCCESS) { pthread_mutex_destroy(&h->mu); free(h); return NULL; }
  if (cuvsCagraIndexParamsCreate(&h->ip) != CUVS_SUCCESS) goto fail;
  if (hipMalloc(&dx, xb) != hipSuccess || hipMalloc(&dg, gb) != hipSuccess) goto fail;
  if (hipMemcpy(dx, base, xb, hipMemcpyHostToDevice) != hipSuccess ||
      hipMemcpy(dg, graph, gb, hipMemcpyHostToDevice) != hipSuccess) goto fail;
  if (cuvsCagraIndexCreate(&h->idx) != CUVS_SUCCESS) { h->idx = 0; goto fail; }
  {
    long gs[2] = {n, degree}, ds[2] = {n, dim};
    DLManagedTensor gt, dt;
    sk_tensor(&gt, dg, 1, gs, kDLUInt, 32);
    sk_tensor(&dt, dx, 1, ds, kDLFloat, 32);
    if (cuvsCagraIndexFromArgs(h->res, L2Expanded, &gt, &dt, h->idx) != CUVS_SUCCESS ||
        cuvsStreamSync(h->res) != CUVS_SUCCESS) goto fail;
  }
  h->d_dataset = dx; h->d_graph = dg; h->n = n; h->dim = dim;
  return h;
fail:
  fprintf(stderr, "[seekdb_cuvs] from_graph failed: %s\n", cuvsGetLastErrorText());
  if (h->idx) cuvsCagraIndexDestroy(h->idx);
  if (h->ip) cuvsCagraIndexParamsDestroy(h->ip);
  if (dx) hipFree(dx);
  if (dg) hipFree(dg);
  cuvsResourcesDestroy(h->res);
  pthread_mutex_destroy(&h->mu);
  free(h);
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
