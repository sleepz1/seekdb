# AE-01: hipVS 加速 HNSW 索引构建

本分支增加了可选的 AMD Radeon/ROCm hipVS 构建后端和可复现实验。默认构建行为不变；只有同时编译 `OB_ENABLE_HIPVS=ON`、设置 `OB_HIPVS_HNSW=1`，并提供 `OB_HIPVS_HNSW_ARTIFACT` 输出路径时才会触发 GPU 构建。

## 设计

- 输入限定为 FP32、L2、静态批量构建，避免隐式改变 seekdb 已支持的量化、稀疏和增量路径。
- hipVS 先用 `hnsw::to_cagra_params` 和 `cagra::build` 在 Radeon GPU 上构图，再用 `hnsw::from_cagra` 配置 `HnswHierarchy::GPU` 构建层次结构，并通过官方 `hnsw::serialize` 产出 artifact。
- hipVS 的序列化格式与 VSAG 格式不同，因此运行时保留 VSAG 作为 seekdb serving index；GPU artifact 用于 hipVS/hnswlib 兼容的验证与部署流程。任何 ROCm/设备异常都会回退 VSAG，不影响数据库建索引。
- `tools/hipvs/hnsw_benchmark.cpp` 固定随机种子、数据规模、维度、M 和 efConstruction，分别测量 `HnswHierarchy::CPU` 与 `GPU`，输出 wall time 和 speedup。

## AMD 环境验证

```bash
rocminfo
hipcc --version
cmake -S . -B build -DOB_ENABLE_HIPVS=ON
cmake --build build --target hipvs_hnsw_benchmark -j
./build/tools/hipvs/hipvs_hnsw_benchmark 100000 128 32 200 ./artifacts
```

提测时应一并记录完整 benchmark stdout、GPU 型号、驱动、ROCm 版本和 hipVS commit。若 `rocminfo` 或 benchmark 失败，不应填写虚构的加速比。
