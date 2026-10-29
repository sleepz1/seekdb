# AE-01 hipVS HNSW benchmark

This benchmark uses the official hipVS HNSW API. It builds one CAGRA graph and
converts it twice: once with `HnswHierarchy::CPU` and once with
`HnswHierarchy::GPU`. Both artifacts use the same deterministic FP32/L2 data,
`M`, and `efConstruction`; the output reports wall time and `CPU/GPU` speedup.

The seekdb runtime path is opt-in. Build with `-DOB_ENABLE_HIPVS=ON`, set
`OB_HIPVS_HNSW=1`, and set `OB_HIPVS_HNSW_ARTIFACT` to an output path. If ROCm,
hipVS, or a Radeon device is unavailable, seekdb keeps the existing VSAG build.
The generated artifact is the hipVS/hnswlib format and is not silently loaded as
a VSAG index because the two serialization formats are different.

## Build and run on Radeon/ROCm

```bash
cmake -S . -B build -DOB_ENABLE_HIPVS=ON \
  -DCMAKE_PREFIX_PATH="$ROCM_PATH/lib/cmake"
cmake --build build --target hipvs_hnsw_benchmark -j
./build/tools/hipvs/hipvs_hnsw_benchmark 100000 128 32 200 ./artifacts
```

If the distribution does not expose a prebuilt target, compile
`hnsw_benchmark.cpp` with the hipVS `cuvs::cuvs` target and the same include/link
flags used by the seekdb build. Record the complete stdout line together with
`rocminfo`, ROCm version, hipVS commit, GPU model, and driver version.

Do not report a speedup when the command exits non-zero or when no Radeon GPU is
visible. The benchmark result is hardware-dependent and should be attached to
the AE-01 submission as evidence.
