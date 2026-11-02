// Custom GPU CAGRA graph builder: exact kNN (hipBLAS SGEMM + LDS top-K select) + CAGRA-style
// rank-based detour pruning + reverse-edge merge. All heavy work on the GPU.
#include <hip/hip_runtime.h>
#include <hipblas/hipblas.h>
#include <stdint.h>
#include <stdio.h>
#include <math.h>

#define SK_SEL_THREADS 256
#define SK_SEL_CAP 2048   // LDS sort buffer (top-K + candidates), power of two

// squared norms
__global__ void sk_norms(const float* __restrict__ x, long n, int dim, float* __restrict__ out) {
  long i = (long)blockIdx.x * blockDim.y + threadIdx.y;
  if (i >= n) return;
  const float* r = x + i * dim;
  float s = 0.f;
  for (int j = threadIdx.x; j < dim; j += blockDim.x) { float v = r[j]; s += v * v; }
  for (int o = warpSize / 2; o > 0; o >>= 1) s += __shfl_down(s, o);
  if (threadIdx.x == 0) out[i] = s;
}

__device__ __forceinline__ void sk_bitonic(float* kd, uint32_t* ki, int len) {
  // ascending sort of len (pow2) items in LDS by (kd, ki)
  for (int size = 2; size <= len; size <<= 1) {
    for (int stride = size >> 1; stride > 0; stride >>= 1) {
      __syncthreads();
      for (int t = threadIdx.x; t < (len >> 1); t += blockDim.x) {
        int lo = 2 * t - (t & (stride - 1));
        int hi = lo + stride;
        bool up = ((lo & size) == 0);
        float a = kd[lo], b = kd[hi];
        uint32_t ia = ki[lo], ib = ki[hi];
        bool gt = (a > b) || (a == b && ia > ib);
        if (gt == up) { kd[lo] = b; kd[hi] = a; ki[lo] = ib; ki[hi] = ia; }
      }
    }
  }
  __syncthreads();
}

// One block per query row: dot[row][0..n) holds q.x; dist = xn[c] - 2*dot. Writes K nearest ids (excluding self).
__global__ void __launch_bounds__(SK_SEL_THREADS)
sk_select(const float* __restrict__ dot, long n, long row0, const float* __restrict__ xn,
          int K, uint32_t* __restrict__ out) {
  __shared__ float kd[SK_SEL_CAP];
  __shared__ uint32_t ki[SK_SEL_CAP];
  __shared__ int cnt;
  __shared__ float thr;
  const long r = blockIdx.x;
  const long self = row0 + r;
  const float* row = dot + r * n;
  for (int t = threadIdx.x; t < SK_SEL_CAP; t += blockDim.x) { kd[t] = INFINITY; ki[t] = 0xffffffffu; }
  if (threadIdx.x == 0) { cnt = K; thr = INFINITY; }
  __syncthreads();
  const int limit = SK_SEL_CAP - SK_SEL_THREADS;
  for (long base = 0; base < n; base += SK_SEL_THREADS) {
    long c = base + threadIdx.x;
    if (c < n && c != self) {
      float d = xn[c] - 2.f * row[c];
      if (!(d != d) && d < thr) {   // skip NaN
        int p = atomicAdd(&cnt, 1);
        kd[p] = d; ki[p] = (uint32_t)c;
      }
    }
    __syncthreads();
    const int cur = cnt;
    __syncthreads();
    if (cur > limit) {
      sk_bitonic(kd, ki, SK_SEL_CAP);
      for (int t = K + threadIdx.x; t < SK_SEL_CAP; t += blockDim.x) { kd[t] = INFINITY; ki[t] = 0xffffffffu; }
      if (threadIdx.x == 0) { cnt = K; thr = kd[K - 1]; }
      __syncthreads();
    }
  }
  sk_bitonic(kd, ki, SK_SEL_CAP);
  for (int t = threadIdx.x; t < K; t += blockDim.x) out[self * K + t] = ki[t];
}

// Rank-based detour pruning (CAGRA optimize, first phase). One block per node.
#define SK_HASH 1024
__global__ void __launch_bounds__(256)
sk_prune(const uint32_t* __restrict__ knn, long n, int K, int D, uint32_t* __restrict__ out) {
  __shared__ uint32_t la[512];
  __shared__ uint32_t cnt[512];
  __shared__ uint32_t hk[SK_HASH];
  __shared__ uint16_t hv[SK_HASH];
  const long a = blockIdx.x;
  const uint32_t* row = knn + a * K;
  for (int t = threadIdx.x; t < SK_HASH; t += blockDim.x) hk[t] = 0xffffffffu;
  for (int t = threadIdx.x; t < K; t += blockDim.x) { la[t] = row[t]; cnt[t] = 0; }
  __syncthreads();
  for (int t = threadIdx.x; t < K; t += blockDim.x) {
    uint32_t key = la[t];
    if (key == 0xffffffffu) continue;
    uint32_t h = (key * 2654435761u) >> 22;
    while (true) {
      uint32_t prev = atomicCAS(&hk[h], 0xffffffffu, key);
      if (prev == 0xffffffffu) { hv[h] = (uint16_t)t; break; }
      if (prev == key) break;  // duplicate id: keep first rank
      h = (h + 1) & (SK_HASH - 1);
    }
  }
  __syncthreads();
  // duplicate ids within la (should not happen) are ignored: hv holds one rank.
  for (int kAD = 1; kAD < K - 1; kAD++) {
    uint32_t d = la[kAD];
    if (d == 0xffffffffu || d >= n) continue;
    const uint32_t* ld = knn + (long)d * K;
    for (int kDB = threadIdx.x; kDB < kAD; kDB += blockDim.x) {
      uint32_t b = ld[kDB];
      uint32_t h = (b * 2654435761u) >> 22;
      while (true) {
        uint32_t key = hk[h];
        if (key == 0xffffffffu) break;
        if (key == b) { int r = hv[h]; if (r > kAD) atomicAdd(&cnt[r], 1u); break; }
        h = (h + 1) & (SK_HASH - 1);
      }
    }
  }
  __syncthreads();
  for (int k = threadIdx.x; k < K; k += blockDim.x) {
    uint32_t ck = cnt[k];
    int pos = 0;
    for (int j = 0; j < K; j++) { uint32_t cj = cnt[j]; pos += (cj < ck) || (cj == ck && j < k); }
    if (pos < D) out[a * D + pos] = la[k];
  }
}

__global__ void sk_rev(const uint32_t* __restrict__ g, long n, int D, int k,
                       uint32_t* __restrict__ rev, uint32_t* __restrict__ rc) {
  long s = (long)blockIdx.x * blockDim.x + threadIdx.x;
  if (s >= n) return;
  uint32_t d = g[s * D + k];
  if (d >= n) return;
  uint32_t p = atomicAdd(&rc[d], 1u);
  if (p < (uint32_t)D) rev[(long)d * D + p] = (uint32_t)s;
}

__global__ void sk_merge(uint32_t* __restrict__ g, const uint32_t* __restrict__ rev,
                         const uint32_t* __restrict__ rc, long n, int D) {
  long i = (long)blockIdx.x * blockDim.x + threadIdx.x;
  if (i >= n) return;
  uint32_t* fw = g + i * D;
  const int prot = D / 2;
  int kr = rc[i] < (uint32_t)D ? (int)rc[i] : D;
  while (kr) {
    kr--;
    uint32_t r = rev[i * D + kr];
    if (r >= n) continue;
    int pos = D;
    for (int j = 0; j < D; j++) if (fw[j] == r) { pos = j; break; }
    if (pos < prot) continue;
    int ns = pos - prot;
    if (pos >= D) ns = D - prot - 1;
    for (int j = prot + ns; j > prot; j--) fw[j] = fw[j - 1];
    fw[prot] = r;
  }
}

extern "C" int sk_build_cagra_graph(const float* d_x, long n, int dim, int K, int D,
                                    uint32_t* d_graph, hipStream_t st, double* t_knn, double* t_prune) {
  if (n <= K + 1 || K > 512 || D > K) return 1;
  hipblasHandle_t hb;
  if (hipblasCreate(&hb) != HIPBLAS_STATUS_SUCCESS) return 2;
  hipblasSetStream(hb, st);
  int rc = 0;
  float* xn = nullptr; float* tile = nullptr; uint32_t* knn = nullptr; uint32_t* rev = nullptr; uint32_t* rcnt = nullptr;
  long tile_elems = 128L << 20;            // 512 MB of fp32 distances
  long bq = tile_elems / n; if (bq < 64) bq = 64; if (bq > n) bq = n;
  hipEvent_t e0, e1, e2;
  hipEventCreate(&e0); hipEventCreate(&e1); hipEventCreate(&e2);
  if (hipMalloc(&xn, sizeof(float) * n) != hipSuccess ||
      hipMalloc(&tile, sizeof(float) * bq * n) != hipSuccess ||
      hipMalloc(&knn, sizeof(uint32_t) * n * K) != hipSuccess) { rc = 3; goto done; }
  hipEventRecord(e0, st);
  sk_norms<<<dim3((n + 7) / 8), dim3(32, 8), 0, st>>>(d_x, n, dim, xn);
  for (long r0 = 0; r0 < n; r0 += bq) {
    long m = n - r0 < bq ? n - r0 : bq;
    const float one = 1.f, zero = 0.f;
    // tile(row-major m x n) = Xq . X^T  ==  col-major (n x m) = X'^T(n x dim) * Xq'(dim x m)
    if (hipblasSgemm(hb, HIPBLAS_OP_T, HIPBLAS_OP_N, (int)n, (int)m, dim, &one, d_x, dim,
                     d_x + r0 * dim, dim, &zero, tile, (int)n) != HIPBLAS_STATUS_SUCCESS) { rc = 4; goto done; }
    sk_select<<<m, SK_SEL_THREADS, 0, st>>>(tile, n, r0, xn, K, knn);
  }
  hipFree(tile); tile = nullptr;
  hipEventRecord(e1, st);
  sk_prune<<<n, 256, 0, st>>>(knn, n, K, D, d_graph);
  hipFree(knn); knn = nullptr;
  if (hipMalloc(&rev, sizeof(uint32_t) * n * D) != hipSuccess || hipMalloc(&rcnt, sizeof(uint32_t) * n) != hipSuccess) { rc = 5; goto done; }
  hipMemsetAsync(rcnt, 0, sizeof(uint32_t) * n, st);
  hipMemsetAsync(rev, 0xff, sizeof(uint32_t) * n * D, st);
  for (int k = 0; k < D; k++) sk_rev<<<(n + 255) / 256, 256, 0, st>>>(d_graph, n, D, k, rev, rcnt);
  sk_merge<<<(n + 127) / 128, 128, 0, st>>>(d_graph, rev, rcnt, n, D);
  hipEventRecord(e2, st);
  if (hipStreamSynchronize(st) != hipSuccess) { rc = 6; goto done; }
  if (hipGetLastError() != hipSuccess) { rc = 7; goto done; }
  { float a = 0, b = 0; hipEventElapsedTime(&a, e0, e1); hipEventElapsedTime(&b, e1, e2);
    if (t_knn) *t_knn = a / 1e3; if (t_prune) *t_prune = b / 1e3; }
done:
  hipStreamSynchronize(st);
  if (xn) hipFree(xn); if (tile) hipFree(tile); if (knn) hipFree(knn); if (rev) hipFree(rev); if (rcnt) hipFree(rcnt);
  hipEventDestroy(e0); hipEventDestroy(e1); hipEventDestroy(e2);
  hipblasDestroy(hb);
  return rc;
}
