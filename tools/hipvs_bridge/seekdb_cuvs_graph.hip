// Custom GPU CAGRA graph builder: exact kNN (hipBLAS SGEMM + LDS top-K select) + CAGRA-style
// rank-based detour pruning + reverse-edge merge. All heavy work on the GPU.
#include <hip/hip_runtime.h>
#include <stdint.h>
#include <stdio.h>
#include <math.h>

#include <stdlib.h>
static void sk_memdbg(const char* tag) {
  static int on = -1;
  if (on < 0) on = getenv("SK_MEMDBG") != nullptr;
  if (!on) return;
  size_t f = 0, t = 0; hipDeviceSynchronize(); hipMemGetInfo(&f, &t);
  fprintf(stderr, "[sk_mem] %s used=%.1fMB\n", tag, (t - f) / 1e6);
}
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

// ---- distance tile: T[r][c] = |x_c|^2 - 2 <q_r, x_c>, r in [r0, r0+m), c in [c0, c0+w)
#define GT_BM 128
#define GT_BN 128
#define GT_BK 32
template <bool VEC>
__global__ void __launch_bounds__(256)
sk_dist_tile(const float* __restrict__ x, long n, int dim, const float* __restrict__ xn,
             long r0, int m, long c0, int w, float* __restrict__ tile, int ldt) {
  __shared__ float As[GT_BK][GT_BM + 4];
  __shared__ float Bs[GT_BK][GT_BN + 4];
  const int tx = threadIdx.x & 15, ty = threadIdx.x >> 4;
  const int bm = blockIdx.y * GT_BM, bn = blockIdx.x * GT_BN;
  float acc[8][8];
#pragma unroll
  for (int i = 0; i < 8; i++)
#pragma unroll
    for (int j = 0; j < 8; j++) acc[i][j] = 0.f;
  for (int k0 = 0; k0 < dim; k0 += GT_BK) {
#pragma unroll
    for (int s4 = 0; s4 < 4; s4++) {
      const int idx = threadIdx.x + 256 * s4;
      const int row = idx >> 3, c4 = (idx & 7) * 4;
      const int kk = k0 + c4;
      const bool qa_ok = (bm + row) < m, cb_ok = (bn + row) < w;
      const float* pa = x + (r0 + bm + row) * (long)dim + kk;
      const float* pb = x + (c0 + bn + row) * (long)dim + kk;
      float4 va = make_float4(0.f, 0.f, 0.f, 0.f), vb = va;
      if (VEC) {
        if (qa_ok && kk < dim) va = *reinterpret_cast<const float4*>(pa);
        if (cb_ok && kk < dim) vb = *reinterpret_cast<const float4*>(pb);
      } else {
        if (qa_ok) { if (kk < dim) va.x = pa[0]; if (kk + 1 < dim) va.y = pa[1]; if (kk + 2 < dim) va.z = pa[2]; if (kk + 3 < dim) va.w = pa[3]; }
        if (cb_ok) { if (kk < dim) vb.x = pb[0]; if (kk + 1 < dim) vb.y = pb[1]; if (kk + 2 < dim) vb.z = pb[2]; if (kk + 3 < dim) vb.w = pb[3]; }
      }
      As[c4 + 0][row] = va.x; As[c4 + 1][row] = va.y; As[c4 + 2][row] = va.z; As[c4 + 3][row] = va.w;
      Bs[c4 + 0][row] = vb.x; Bs[c4 + 1][row] = vb.y; Bs[c4 + 2][row] = vb.z; Bs[c4 + 3][row] = vb.w;
    }
    __syncthreads();
#pragma unroll
    for (int kk = 0; kk < GT_BK; kk++) {
      float a[8], b[8];
#pragma unroll
      for (int i = 0; i < 8; i++) a[i] = As[kk][ty + 16 * i];
#pragma unroll
      for (int j = 0; j < 8; j++) b[j] = Bs[kk][tx + 16 * j];
#pragma unroll
      for (int i = 0; i < 8; i++)
#pragma unroll
        for (int j = 0; j < 8; j++) acc[i][j] = fmaf(a[i], b[j], acc[i][j]);
    }
    __syncthreads();
  }
#pragma unroll
  for (int i = 0; i < 8; i++) {
    int rr = bm + ty + 16 * i;
    if (rr >= m) continue;
#pragma unroll
    for (int j = 0; j < 8; j++) {
      int cc = bn + tx + 16 * j;
      if (cc < w) tile[(long)rr * ldt + cc] = xn[c0 + cc] - 2.f * acc[i][j];
    }
  }
}

__global__ void sk_state_init(float* sd, uint32_t* si, long cnt) {
  long i = (long)blockIdx.x * blockDim.x + threadIdx.x;
  if (i < cnt) { sd[i] = INFINITY; si[i] = 0xffffffffu; }
}

// Merge one tile chunk of candidates into the per-row running top-K (sorted) state.
__global__ void __launch_bounds__(SK_SEL_THREADS)
sk_select_chunk(const float* __restrict__ tile, int ldt, int w, long c0, long row0, int K,
                float* __restrict__ sd, uint32_t* __restrict__ si) {
  __shared__ float kd[SK_SEL_CAP];
  __shared__ uint32_t ki[SK_SEL_CAP];
  __shared__ int cnt;
  __shared__ float thr;
  const long r = blockIdx.x;
  const long self = row0 + r;
  const float* row = tile + r * ldt;
  float* rsd = sd + r * K; uint32_t* rsi = si + r * K;
  for (int t = threadIdx.x; t < SK_SEL_CAP; t += blockDim.x) {
    if (t < K) { kd[t] = rsd[t]; ki[t] = rsi[t]; } else { kd[t] = INFINITY; ki[t] = 0xffffffffu; }
  }
  if (threadIdx.x == 0) { cnt = K; }
  __syncthreads();
  if (threadIdx.x == 0) thr = kd[K - 1];
  __syncthreads();
  const int limit = SK_SEL_CAP - SK_SEL_THREADS;
  for (int base = 0; base < w; base += SK_SEL_THREADS) {
    int c = base + threadIdx.x;
    if (c < w && c0 + c != self) {
      float d = row[c];
      if (d < thr) {   // NaN compares false
        int p = atomicAdd(&cnt, 1);
        kd[p] = d; ki[p] = (uint32_t)(c0 + c);
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
  if (cnt > K) sk_bitonic(kd, ki, SK_SEL_CAP);
  for (int t = threadIdx.x; t < K; t += blockDim.x) { rsd[t] = kd[t]; rsi[t] = ki[t]; }
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

extern "C" int sk_build_cagra_graph(float** pd_x, const float* h_x, long n, int dim, int K, int D,
                                    uint32_t** pd_graph, hipStream_t st, double* t_knn, double* t_prune) {
  // d_x: device copy of the dataset (n x dim). To keep peak VRAM at the level of the final index
  // (dataset + graph), intermediate results are staged in pinned host memory and the dataset is
  // released while pruning, then re-uploaded from h_x before returning.
  if (n <= K + 1 || K > 512 || D > K || K > SK_SEL_CAP - SK_SEL_THREADS || h_x == nullptr) return 1;
  float* d_x = *pd_x;
  uint32_t* d_graph = nullptr;
  *pd_graph = nullptr;
  int rc = 0;
  float* xn = nullptr; float* tile = nullptr; float* sd = nullptr; uint32_t* si = nullptr;
  uint32_t* knn = nullptr; uint32_t* rev = nullptr; uint32_t* rcnt = nullptr;
  uint32_t* h_knn = nullptr; uint32_t* h_graph = nullptr; uint32_t* h_graph_dev = nullptr;
  const long BQ = 512, CC = 32768;
  long bq = BQ < n ? BQ : n;
  long cc = CC < n ? CC : n;
  hipEvent_t e0, e1, e2;
  hipEventCreate(&e0); hipEventCreate(&e1); hipEventCreate(&e2);
  if (hipHostMalloc((void**)&h_knn, sizeof(uint32_t) * n * K, hipHostMallocDefault) != hipSuccess ||
      hipHostMalloc((void**)&h_graph, sizeof(uint32_t) * n * D, hipHostMallocMapped) != hipSuccess ||
      hipHostGetDevicePointer((void**)&h_graph_dev, h_graph, 0) != hipSuccess) { rc = 2; goto done; }
  if (hipMalloc(&xn, sizeof(float) * n) != hipSuccess ||
      hipMalloc(&tile, sizeof(float) * bq * cc) != hipSuccess ||
      hipMalloc(&sd, sizeof(float) * bq * K) != hipSuccess ||
      hipMalloc(&si, sizeof(uint32_t) * bq * K) != hipSuccess) { rc = 3; goto done; }
  sk_memdbg("after knn allocs");
  hipEventRecord(e0, st);
  sk_norms<<<dim3((n + 7) / 8), dim3(32, 8), 0, st>>>(d_x, n, dim, xn);
  for (long r0 = 0; r0 < n; r0 += bq) {
    int m = (int)(n - r0 < bq ? n - r0 : bq);
    sk_state_init<<<(m * K + 255) / 256, 256, 0, st>>>(sd, si, (long)m * K);
    for (long c0 = 0; c0 < n; c0 += cc) {
      int w = (int)(n - c0 < cc ? n - c0 : cc);
      dim3 g((w + GT_BN - 1) / GT_BN, (m + GT_BM - 1) / GT_BM);
      if ((dim & 3) == 0) sk_dist_tile<true><<<g, 256, 0, st>>>(d_x, n, dim, xn, r0, m, c0, w, tile, (int)cc);
      else sk_dist_tile<false><<<g, 256, 0, st>>>(d_x, n, dim, xn, r0, m, c0, w, tile, (int)cc);
      sk_select_chunk<<<m, SK_SEL_THREADS, 0, st>>>(tile, (int)cc, w, c0, r0, K, sd, si);
    }
    if (hipMemcpyAsync(h_knn + r0 * K, si, sizeof(uint32_t) * m * K, hipMemcpyDeviceToHost, st) != hipSuccess) { rc = 4; goto done; }
  }
  hipEventRecord(e1, st);
  if (hipStreamSynchronize(st) != hipSuccess || hipGetLastError() != hipSuccess) { rc = 8; goto done; }
  sk_memdbg("after knn");
  hipFree(tile); tile = nullptr; hipFree(sd); sd = nullptr; hipFree(si); si = nullptr; hipFree(xn); xn = nullptr;
  hipFree(d_x); d_x = nullptr; *pd_x = nullptr;
  if (hipMalloc(&knn, sizeof(uint32_t) * n * K) != hipSuccess ||
      hipMemcpyAsync(knn, h_knn, sizeof(uint32_t) * n * K, hipMemcpyHostToDevice, st) != hipSuccess) { rc = 9; goto done; }
  sk_prune<<<n, 256, 0, st>>>(knn, n, K, D, h_graph_dev);
  if (hipStreamSynchronize(st) != hipSuccess) { rc = 9; goto done; }
  sk_memdbg("after prune");
  hipFree(knn); knn = nullptr;
  if (hipMalloc(&d_graph, sizeof(uint32_t) * n * D) != hipSuccess ||
      hipMemcpyAsync(d_graph, h_graph, sizeof(uint32_t) * n * D, hipMemcpyHostToDevice, st) != hipSuccess) { rc = 10; goto done; }
  if (hipMalloc(&rev, sizeof(uint32_t) * n * D) != hipSuccess || hipMalloc(&rcnt, sizeof(uint32_t) * n) != hipSuccess) { rc = 5; goto done; }
  hipMemsetAsync(rcnt, 0, sizeof(uint32_t) * n, st);
  hipMemsetAsync(rev, 0xff, sizeof(uint32_t) * n * D, st);
  for (int k = 0; k < D; k++) sk_rev<<<(n + 255) / 256, 256, 0, st>>>(d_graph, n, D, k, rev, rcnt);
  sk_merge<<<(n + 127) / 128, 128, 0, st>>>(d_graph, rev, rcnt, n, D);
  hipEventRecord(e2, st);
  if (hipStreamSynchronize(st) != hipSuccess) { rc = 6; goto done; }
  if (hipGetLastError() != hipSuccess) { rc = 7; goto done; }
  sk_memdbg("after merge");
  hipFree(rev); rev = nullptr; hipFree(rcnt); rcnt = nullptr;
  if (hipMalloc(&d_x, sizeof(float) * n * dim) != hipSuccess ||
      hipMemcpyAsync(d_x, h_x, sizeof(float) * n * dim, hipMemcpyHostToDevice, st) != hipSuccess ||
      hipStreamSynchronize(st) != hipSuccess) { rc = 11; goto done; }
  *pd_x = d_x; d_x = nullptr;
  *pd_graph = d_graph; d_graph = nullptr;
  { float a = 0, b = 0; hipEventElapsedTime(&a, e0, e1); hipEventElapsedTime(&b, e1, e2);
    if (t_knn) *t_knn = a / 1e3; if (t_prune) *t_prune = b / 1e3; }
done:
  hipStreamSynchronize(st);
  if (xn) hipFree(xn); if (tile) hipFree(tile); if (sd) hipFree(sd); if (si) hipFree(si);
  if (knn) hipFree(knn); if (rev) hipFree(rev); if (rcnt) hipFree(rcnt);
  if (d_graph) hipFree(d_graph);
  if (d_x) { hipFree(d_x); *pd_x = nullptr; }
  if (h_knn) hipHostFree(h_knn); if (h_graph) hipHostFree(h_graph);
  hipEventDestroy(e0); hipEventDestroy(e1); hipEventDestroy(e2);
  return rc;
}
