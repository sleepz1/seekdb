// Custom GPU CAGRA graph builder: exact kNN (hipBLAS SGEMM + LDS top-K select) + CAGRA-style
// rank-based detour pruning + reverse-edge merge. All heavy work on the GPU.
#include <hip/hip_runtime.h>
#include <stdint.h>
#include <stdio.h>
#include <math.h>

#include <stdlib.h>
#include <string.h>
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
__device__ __forceinline__ uint32_t sk_perm(uint32_t c, unsigned long long pa, unsigned long long pb, long n) {
  return (uint32_t)(((unsigned long long)c * pa + pb) % (unsigned long long)n);
}
__global__ void __launch_bounds__(256)
sk_prune(const uint32_t* __restrict__ knn, long n, int K, int D, uint32_t* __restrict__ out,
         unsigned long long pa, unsigned long long pb) {
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
    if (pos < D) out[(long)sk_perm((uint32_t)a, pa, pb, n) * D + pos] = la[k] < n ? sk_perm(la[k], pa, pb, n) : la[k];
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

// ---------------- fp16 WMMA kNN (gfx11) ----------------
// Dataset is converted to fp16 (scaled by a power of two, zero padded to dimp % 16 == 0) in a
// pseudo-random column order p(c) = (c * pa + pb) % n, so that the running top-K threshold
// tightens quickly regardless of the physical data order. Distances are |x_c|^2 - 2 <q, x_c>
// accumulated in fp32. A fused epilogue keeps only candidates below the row's current K-th
// distance; candidates are merged into the sorted per-row state after each column chunk.
typedef _Float16 sk_half16 __attribute__((ext_vector_type(16)));
typedef _Float16 sk_half8 __attribute__((ext_vector_type(8)));
typedef float sk_float8 __attribute__((ext_vector_type(8)));
#define WB_M 128
#define WB_N 128
#define WB_K 64
#define WB_LD (WB_K + 8)
#define SK_CAP 1536
#define SK_RB 8192

__global__ void sk_absmax(const float* __restrict__ x, long cnt, unsigned* __restrict__ out) {
  unsigned m = 0;
  for (long i = (long)blockIdx.x * blockDim.x + threadIdx.x; i < cnt; i += (long)gridDim.x * blockDim.x) {
    float v = fabsf(x[i]);
    unsigned u = __float_as_uint(v);
    if (v == v && u > m && v != INFINITY) m = u;
  }
  for (int o = warpSize / 2; o > 0; o >>= 1) { unsigned t = __shfl_down(m, o); m = t > m ? t : m; }
  if ((threadIdx.x & (warpSize - 1)) == 0) atomicMax(out, m);
}

// one wave per output row c: xp[c] = half(scale * x[p(c)]), xn[c] = |xp[c]|^2
__global__ void sk_to_half(const float* __restrict__ x, long n, int dim, int dimp,
                           unsigned long long pa, unsigned long long pb, float scale,
                           _Float16* __restrict__ xp, float* __restrict__ xn) {
  long c = (long)blockIdx.x * blockDim.y + threadIdx.y;
  if (c >= n) return;
  long src = (long)(((unsigned long long)c * pa + pb) % (unsigned long long)n);
  const float* r = x + src * dim;
  _Float16* o = xp + c * dimp;
  float s = 0.f;
  for (int j = threadIdx.x; j < dimp; j += blockDim.x) {
    _Float16 h = (_Float16)(j < dim ? r[j] * scale : 0.f);
    o[j] = h;
    float f = (float)h; s += f * f;
  }
  for (int off = warpSize / 2; off > 0; off >>= 1) s += __shfl_down(s, off);
  if (threadIdx.x == 0) xn[c] = s;
}

__global__ void sk_state_init2(float* sd, uint32_t* si, float* thr, long cnt, int K) {
  long i = (long)blockIdx.x * blockDim.x + threadIdx.x;
  if (i < cnt) { sd[i] = INFINITY; si[i] = 0xffffffffu; if (i % K == 0) thr[i / K] = INFINITY; }
}

__global__ void __launch_bounds__(256)
sk_wmma_filter(const _Float16* __restrict__ xp, int dimp, const float* __restrict__ xn,
               long r0, int m, long c0, int w, const float* __restrict__ thr,
               unsigned* __restrict__ cnt, float* __restrict__ cd, uint32_t* __restrict__ ci) {
  __shared__ _Float16 As[WB_M * WB_LD];
  __shared__ _Float16 Bs[WB_N * WB_LD];
  const int tid = threadIdx.x, lane = tid & 31, wave = tid >> 5;
  const int wr = wave >> 1, wc = wave & 1;
  const int bm = blockIdx.y * WB_M, bn = blockIdx.x * WB_N;
  sk_float8 acc[2][4];
#pragma unroll
  for (int i = 0; i < 2; i++)
#pragma unroll
    for (int j = 0; j < 4; j++)
#pragma unroll
      for (int v = 0; v < 8; v++) acc[i][j][v] = 0.f;
  for (int k0 = 0; k0 < dimp; k0 += WB_K) {
#pragma unroll
    for (int s4 = 0; s4 < 4; s4++) {
      const int idx = tid + 256 * s4;
      const int row = idx >> 3, seg = (idx & 7) * 8;
      const int kk = k0 + seg;
      uint4 va = make_uint4(0, 0, 0, 0), vb = va;
      if (bm + row < m && kk < dimp) va = *reinterpret_cast<const uint4*>(xp + (r0 + bm + row) * (long)dimp + kk);
      if (bn + row < w && kk < dimp) vb = *reinterpret_cast<const uint4*>(xp + (c0 + bn + row) * (long)dimp + kk);
      *reinterpret_cast<uint4*>(As + row * WB_LD + seg) = va;
      *reinterpret_cast<uint4*>(Bs + row * WB_LD + seg) = vb;
    }
    __syncthreads();
    const int kl = dimp - k0 < WB_K ? dimp - k0 : WB_K;
    for (int ks = 0; ks < kl; ks += 16) {
      sk_half16 a[2], b[4];
#pragma unroll
      for (int i = 0; i < 2; i++) {
        const _Float16* p = As + (wr * 32 + i * 16 + (lane & 15)) * WB_LD + ks;
        sk_half8 lo = *reinterpret_cast<const sk_half8*>(p), hi = *reinterpret_cast<const sk_half8*>(p + 8);
#pragma unroll
        for (int t = 0; t < 8; t++) { a[i][t] = lo[t]; a[i][t + 8] = hi[t]; }
      }
#pragma unroll
      for (int j = 0; j < 4; j++) {
        const _Float16* p = Bs + (wc * 64 + j * 16 + (lane & 15)) * WB_LD + ks;
        sk_half8 lo = *reinterpret_cast<const sk_half8*>(p), hi = *reinterpret_cast<const sk_half8*>(p + 8);
#pragma unroll
        for (int t = 0; t < 8; t++) { b[j][t] = lo[t]; b[j][t + 8] = hi[t]; }
      }
#pragma unroll
      for (int i = 0; i < 2; i++)
#pragma unroll
        for (int j = 0; j < 4; j++)
          acc[i][j] = __builtin_amdgcn_wmma_f32_16x16x16_f16_w32(a[i], b[j], acc[i][j]);
    }
    __syncthreads();
  }
  float cn[4]; long cid[4];
#pragma unroll
  for (int j = 0; j < 4; j++) {
    int cl = bn + wc * 64 + j * 16 + (lane & 15);
    cid[j] = cl < w ? c0 + cl : -1;
    cn[j] = cl < w ? xn[c0 + cl] : 0.f;
  }
#pragma unroll
  for (int i = 0; i < 2; i++) {
#pragma unroll
    for (int v = 0; v < 8; v++) {
      const int rl = bm + wr * 32 + i * 16 + 2 * v + (lane >> 4);
      if (rl >= m) continue;
      const float t = thr[rl];
      const long self = r0 + rl;
#pragma unroll
      for (int j = 0; j < 4; j++) {
        float d = cn[j] - 2.f * acc[i][j][v];
        if (cid[j] >= 0 && d < t && cid[j] != self) {
          unsigned p = atomicAdd(&cnt[rl], 1u);
          if (p < SK_CAP) { cd[(long)rl * SK_CAP + p] = d; ci[(long)rl * SK_CAP + p] = (uint32_t)cid[j]; }
        }
      }
    }
  }
}

__device__ __forceinline__ bool sk_lt(float a, uint32_t ia, float b, uint32_t ib) {
  return a < b || (a == b && ia < ib);
}

// Merge the chunk candidates of each row into its sorted top-K state.
__global__ void __launch_bounds__(256)
sk_merge_sel(int K, unsigned* __restrict__ cnt, const float* __restrict__ cd, const uint32_t* __restrict__ ci,
             float* __restrict__ sd, uint32_t* __restrict__ si, float* __restrict__ thr,
             uint32_t* __restrict__ ovf_list, unsigned* __restrict__ ovf_cnt) {
  __shared__ float kd[SK_SEL_CAP];
  __shared__ uint32_t ki[SK_SEL_CAP];
  __shared__ float od[512];
  __shared__ uint32_t oi[512];
  const int r = blockIdx.x;
  const unsigned c = cnt[r];
  if (c == 0) return;
  __syncthreads();
  if (threadIdx.x == 0) cnt[r] = 0;
  if (c > SK_CAP) {
    if (threadIdx.x == 0) { ovf_list[atomicAdd(ovf_cnt, 1u)] = r; atomicAdd(ovf_cnt + 1, 1u); }
    return;
  }
  int P = 1; while (P < (int)c) P <<= 1;
  const float* rcd = cd + (long)r * SK_CAP; const uint32_t* rci = ci + (long)r * SK_CAP;
  for (int t = threadIdx.x; t < P; t += blockDim.x) {
    if (t < (int)c) { kd[t] = rcd[t]; ki[t] = rci[t]; } else { kd[t] = INFINITY; ki[t] = 0xffffffffu; }
  }
  float* rsd = sd + (long)r * K; uint32_t* rsi = si + (long)r * K;
  if (P > 1) sk_bitonic(kd, ki, P); else __syncthreads();
  for (int i = threadIdx.x; i < K; i += blockDim.x) {
    float a = rsd[i]; uint32_t ia = rsi[i];
    int lo = 0, hi = (int)c;  // count cand < a
    while (lo < hi) { int mid = (lo + hi) >> 1; if (sk_lt(kd[mid], ki[mid], a, ia)) lo = mid + 1; else hi = mid; }
    int pos = i + lo;
    if (pos < K) { od[pos] = a; oi[pos] = ia; }
  }
  for (int j = threadIdx.x; j < (int)c; j += blockDim.x) {
    float b = kd[j]; uint32_t ib = ki[j];
    int lo = 0, hi = K;  // count state < b
    while (lo < hi) { int mid = (lo + hi) >> 1; if (sk_lt(rsd[mid], rsi[mid], b, ib)) lo = mid + 1; else hi = mid; }
    int pos = j + lo;
    if (pos < K) { od[pos] = b; oi[pos] = ib; }
  }
  __syncthreads();
  for (int t = threadIdx.x; t < K; t += blockDim.x) { rsd[t] = od[t]; rsi[t] = oi[t]; }
  if (threadIdx.x == 0) thr[r] = od[K - 1];
}

// Exact fallback for rows whose candidate buffer overflowed in this chunk.
__global__ void __launch_bounds__(256)
sk_ovf_row(const _Float16* __restrict__ xp, int dimp, const float* __restrict__ xn, long r0, long c0, int w, int K,
           const uint32_t* __restrict__ ovf_list, unsigned* __restrict__ ovf_cnt,
           float* __restrict__ sd, uint32_t* __restrict__ si, float* __restrict__ thrv) {
  __shared__ float kd[SK_SEL_CAP];
  __shared__ uint32_t ki[SK_SEL_CAP];
  __shared__ float q[2048];
  __shared__ int cnt;
  __shared__ float thr;
  if (blockIdx.x >= *ovf_cnt) return;
  const int r = ovf_list[blockIdx.x];
  const long self = r0 + r;
  for (int t = threadIdx.x; t < dimp; t += blockDim.x) q[t] = (float)xp[self * dimp + t];
  float* rsd = sd + (long)r * K; uint32_t* rsi = si + (long)r * K;
  for (int t = threadIdx.x; t < SK_SEL_CAP; t += blockDim.x) {
    if (t < K) { kd[t] = rsd[t]; ki[t] = rsi[t]; } else { kd[t] = INFINITY; ki[t] = 0xffffffffu; }
  }
  if (threadIdx.x == 0) { cnt = K; thr = rsd[K - 1]; }
  __syncthreads();
  const int limit = SK_SEL_CAP - SK_SEL_THREADS;
  for (int base = 0; base < w; base += SK_SEL_THREADS) {
    int cc = base + threadIdx.x;
    if (cc < w && c0 + cc != self) {
      const _Float16* xr = xp + (c0 + cc) * (long)dimp;
      float dot = 0.f;
      for (int t = 0; t < dimp; t++) dot += q[t] * (float)xr[t];
      float d = xn[c0 + cc] - 2.f * dot;
      if (d < thr) { int p = atomicAdd(&cnt, 1); kd[p] = d; ki[p] = (uint32_t)(c0 + cc); }
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
  if (threadIdx.x == 0) thrv[r] = kd[K - 1];
}

static unsigned long long sk_gcd(unsigned long long a, unsigned long long b) { while (b) { unsigned long long t = a % b; a = b; b = t; } return a; }

// Exact-ish kNN (fp16 inputs, fp32 accumulation) of all n points in permuted index space.
// Writes neighbor ids (permuted space, sorted by distance) of permuted row r to h_knn[r * K].
static int sk_knn_wmma(const float* d_x, long n, int dim, int K, uint32_t* h_knn, hipStream_t st,
                       unsigned long long* ppa, unsigned long long* ppb, long* novf) {
  int rc = 0;
  const int dimp = (dim + 15) / 16 * 16;
  if (dimp > 2048 || K > 512 || n > 0x7fffffffL) return 1;
  unsigned long long pa = (unsigned long long)(n * 0.6180339887498949) | 1ull, pb = (unsigned long long)n / 3;
  while (sk_gcd(pa, (unsigned long long)n) != 1) pa += 2;
  if (getenv("SK_NOPERM")) { pa = 1; pb = 0; }
  *ppa = pa; *ppb = pb;
  _Float16* xp = nullptr; float* xn = nullptr; unsigned* amax = nullptr;
  float* sd = nullptr; uint32_t* si = nullptr; float* thr = nullptr; unsigned* cnt = nullptr;
  float* cd = nullptr; uint32_t* ci = nullptr; uint32_t* ovl = nullptr; unsigned* ovc = nullptr;
  unsigned h_amax = 0; float scale = 1.f;
  const long R = SK_RB < n ? SK_RB : n;
  long tot_ovf = 0;
  unsigned* h_ovc = nullptr;
  if (hipMalloc(&amax, sizeof(unsigned) * 4) != hipSuccess) return 2;
  hipMemsetAsync(amax, 0, sizeof(unsigned) * 4, st);
  sk_absmax<<<1024, 256, 0, st>>>(d_x, n * dim, amax);
  if (hipMemcpyAsync(&h_amax, amax, sizeof(unsigned), hipMemcpyDeviceToHost, st) != hipSuccess ||
      hipStreamSynchronize(st) != hipSuccess) { rc = 2; goto done; }
  {
    float mx; memcpy(&mx, &h_amax, 4);
    if (mx > 0.f && mx == mx) { int e; frexpf(256.f / mx, &e); scale = ldexpf(1.f, e - 1); }
    if (!(scale > 0.f) || scale > 1e30f) scale = 1.f;
  }
  if (hipMalloc(&xp, sizeof(_Float16) * n * dimp) != hipSuccess || hipMalloc(&xn, sizeof(float) * n) != hipSuccess ||
      hipMalloc(&sd, sizeof(float) * R * K) != hipSuccess || hipMalloc(&si, sizeof(uint32_t) * R * K) != hipSuccess ||
      hipMalloc(&thr, sizeof(float) * R) != hipSuccess || hipMalloc(&cnt, sizeof(unsigned) * R) != hipSuccess ||
      hipMalloc(&cd, sizeof(float) * R * SK_CAP) != hipSuccess || hipMalloc(&ci, sizeof(uint32_t) * R * SK_CAP) != hipSuccess ||
      hipMalloc(&ovl, sizeof(uint32_t) * R) != hipSuccess || hipHostMalloc((void**)&h_ovc, sizeof(unsigned) * 64, hipHostMallocDefault) != hipSuccess) { rc = 3; goto done; }
  ovc = amax + 1;
  sk_to_half<<<dim3((n + 7) / 8), dim3(32, 8), 0, st>>>(d_x, n, dim, dimp, pa, pb, scale, xp, xn);
  hipMemsetAsync(cnt, 0, sizeof(unsigned) * R, st);
  for (long r0 = 0; r0 < n; r0 += R) {
    const int m = (int)(n - r0 < R ? n - r0 : R);
    sk_state_init2<<<(m * (long)K + 255) / 256, 256, 0, st>>>(sd, si, thr, (long)m * K, K);
    long c0 = 0, csz = 1024;
    while (c0 < n) {
      const int w = (int)(n - c0 < csz ? n - c0 : csz);
      dim3 g((w + WB_N - 1) / WB_N, (m + WB_M - 1) / WB_M);
      hipMemsetAsync(ovc, 0, sizeof(unsigned), st);
      sk_wmma_filter<<<g, 256, 0, st>>>(xp, dimp, xn, r0, m, c0, w, thr, cnt, cd, ci);
      sk_merge_sel<<<m, 256, 0, st>>>(K, cnt, cd, ci, sd, si, thr, ovl, ovc);
      sk_ovf_row<<<m, 256, 0, st>>>(xp, dimp, xn, r0, c0, w, K, ovl, ovc, sd, si, thr);
      c0 += w;
      csz = c0 < 2048 ? 1024 : c0;
    }
    if (hipMemcpyAsync(h_knn + r0 * K, si, sizeof(uint32_t) * m * K, hipMemcpyDeviceToHost, st) != hipSuccess) { rc = 4; goto done; }
  }
  if (hipMemcpyAsync(h_ovc, amax, sizeof(unsigned) * 4, hipMemcpyDeviceToHost, st) != hipSuccess ||
      hipStreamSynchronize(st) != hipSuccess || hipGetLastError() != hipSuccess) { rc = 8; goto done; }
  tot_ovf = h_ovc[2];
  *novf = tot_ovf;
done:
  hipStreamSynchronize(st);
  if (xp) hipFree(xp); if (xn) hipFree(xn); if (amax) hipFree(amax);
  if (sd) hipFree(sd); if (si) hipFree(si); if (thr) hipFree(thr); if (cnt) hipFree(cnt);
  if (cd) hipFree(cd); if (ci) hipFree(ci); if (ovl) hipFree(ovl); if (h_ovc) hipHostFree(h_ovc);
  return rc;
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
  unsigned long long pa = 1, pb = 0;
  bool done_knn = false;
  hipEvent_t e0, e1, e2;
  hipEventCreate(&e0); hipEventCreate(&e1); hipEventCreate(&e2);
  if (hipHostMalloc((void**)&h_knn, sizeof(uint32_t) * n * K, hipHostMallocDefault) != hipSuccess ||
      hipHostMalloc((void**)&h_graph, sizeof(uint32_t) * n * D, hipHostMallocMapped) != hipSuccess ||
      hipHostGetDevicePointer((void**)&h_graph_dev, h_graph, 0) != hipSuccess) { rc = 2; goto done; }
  hipEventRecord(e0, st);
  if (!getenv("SK_KNN_FP32")) {
    long novf = 0;
    int r2 = sk_knn_wmma(d_x, n, dim, K, h_knn, st, &pa, &pb, &novf);
    if (r2 == 0) done_knn = true; else { pa = 1; pb = 0; }
    if (getenv("SK_MEMDBG")) fprintf(stderr, "[sk] wmma knn rc=%d ovf=%ld\n", r2, novf);
  }
  if (!done_knn) {
  if (hipMalloc(&xn, sizeof(float) * n) != hipSuccess ||
      hipMalloc(&tile, sizeof(float) * bq * cc) != hipSuccess ||
      hipMalloc(&sd, sizeof(float) * bq * K) != hipSuccess ||
      hipMalloc(&si, sizeof(uint32_t) * bq * K) != hipSuccess) { rc = 3; goto done; }
  sk_memdbg("after knn allocs");
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
  }
  hipEventRecord(e1, st);
  if (hipStreamSynchronize(st) != hipSuccess || hipGetLastError() != hipSuccess) { rc = 8; goto done; }
  sk_memdbg("after knn");
  hipFree(tile); tile = nullptr; hipFree(sd); sd = nullptr; hipFree(si); si = nullptr; hipFree(xn); xn = nullptr;
  hipFree(d_x); d_x = nullptr; *pd_x = nullptr;
  if (hipMalloc(&knn, sizeof(uint32_t) * n * K) != hipSuccess ||
      hipMemcpyAsync(knn, h_knn, sizeof(uint32_t) * n * K, hipMemcpyHostToDevice, st) != hipSuccess) { rc = 9; goto done; }
  sk_prune<<<n, 256, 0, st>>>(knn, n, K, D, h_graph_dev, pa, pb);
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
