// AE-01 benchmark: compare hipVS HNSW hierarchy construction on an AMD GPU.
// The CAGRA graph is held constant; only the HNSW hierarchy builder changes.
// This makes the reported speedup attributable to the hipVS HNSW path.
#ifndef CUVS_BUILD_CAGRA_HNSWLIB
#define CUVS_BUILD_CAGRA_HNSWLIB 1
#endif
#include <cuvs/neighbors/hnsw.hpp>
#include <raft/core/device_resources.hpp>
#include <raft/core/host_mdarray.hpp>
#include <raft/matrix/copy.cuh>
#include <raft/random/make_blobs.cuh>

#include <chrono>
#include <cstdlib>
#include <filesystem>
#include <iomanip>
#include <iostream>
#include <string>

namespace {
using clock_type = std::chrono::steady_clock;

struct Result {
  double milliseconds = 0.0;
  std::string artifact;
};

Result build_once(raft::resources& resources,
                  raft::device_matrix<float, int64_t>& dataset,
                  int max_degree,
                  int ef_construction,
                  cuvs::neighbors::hnsw::HnswHierarchy hierarchy,
                  const std::string& artifact)
{
  const auto start = clock_type::now();
  auto cagra_params = cuvs::neighbors::hnsw::to_cagra_params(
      dataset.extents(), max_degree, ef_construction);
  auto cagra_index = cuvs::neighbors::cagra::build(
      resources, cagra_params, dataset);
  cuvs::neighbors::hnsw::index_params hnsw_params;
  hnsw_params.hierarchy = hierarchy;
  hnsw_params.ef_construction = ef_construction;
  auto hnsw_index = cuvs::neighbors::hnsw::from_cagra(
      resources, hnsw_params, cagra_index);
  cuvs::neighbors::hnsw::serialize(resources, artifact, *hnsw_index);
  raft::resource::sync_stream(resources,
                              raft::resource::get_cuda_stream(resources));
  const auto end = clock_type::now();
  return {std::chrono::duration<double, std::milli>(end - start).count(), artifact};
}
}  // namespace

int main(int argc, char** argv)
{
  const int64_t rows = argc > 1 ? std::atoll(argv[1]) : 100000;
  const int64_t dim = argc > 2 ? std::atoll(argv[2]) : 128;
  const int max_degree = argc > 3 ? std::atoi(argv[3]) : 32;
  const int ef_construction = argc > 4 ? std::atoi(argv[4]) : 200;
  const std::string output_dir = argc > 5 ? argv[5] : ".";
  if (rows <= 0 || dim <= 0 || max_degree <= 0 || ef_construction <= 0) {
    std::cerr << "usage: hnsw_benchmark [rows] [dim] [M] [efConstruction] [output_dir]\n";
    return 2;
  }

  try {
    std::filesystem::create_directories(output_dir);
    raft::resources resources;
    auto dataset = raft::make_device_matrix<float, int64_t>(resources, rows, dim);
    raft::random::RngState rng(20261029ULL);
    raft::random::uniform(resources, rng,
                          raft::make_device_vector_view(dataset.data_handle(), dataset.size()),
                          -1.0f, 1.0f);
    auto cpu = build_once(resources, dataset, max_degree, ef_construction,
                          cuvs::neighbors::hnsw::HnswHierarchy::CPU,
                          output_dir + "/hnsw_cpu.bin");
    auto gpu = build_once(resources, dataset, max_degree, ef_construction,
                          cuvs::neighbors::hnsw::HnswHierarchy::GPU,
                          output_dir + "/hnsw_gpu.bin");
    std::cout << std::fixed << std::setprecision(3)
              << "rows=" << rows << ",dim=" << dim
              << ",M=" << max_degree
              << ",efConstruction=" << ef_construction
              << ",cpu_hierarchy_ms=" << cpu.milliseconds
              << ",gpu_hierarchy_ms=" << gpu.milliseconds
              << ",speedup=" << (cpu.milliseconds / gpu.milliseconds)
              << ",cpu_artifact=" << cpu.artifact
              << ",gpu_artifact=" << gpu.artifact << '\n';
    return 0;
  } catch (const std::exception& error) {
    std::cerr << "hipVS benchmark failed: " << error.what() << '\n';
    return 1;
  }
}
