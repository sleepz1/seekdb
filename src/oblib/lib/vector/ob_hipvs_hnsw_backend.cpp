#include "ob_hipvs_hnsw_backend.h"

#include <cstdlib>
#include <cstring>

#if defined(OB_ENABLE_HIPVS)
#include <raft/core/device_resources.hpp>
#include <raft/core/host_mdarray.hpp>
#include <raft/matrix/copy.cuh>
#ifndef CUVS_BUILD_CAGRA_HNSWLIB
#define CUVS_BUILD_CAGRA_HNSWLIB 1
#endif
#include <cuvs/neighbors/hnsw.hpp>
#endif

namespace oceanbase {
namespace common {
namespace obvsag {

bool hipvs_hnsw_enabled()
{
  const char *value = std::getenv("OB_HIPVS_HNSW");
  return value != nullptr && (std::strcmp(value, "1") == 0 ||
                              std::strcmp(value, "true") == 0 ||
                              std::strcmp(value, "on") == 0);
}

#if defined(OB_ENABLE_HIPVS)
bool build_hipvs_hnsw_artifact(const float *vectors,
                               int64_t row_count,
                               int64_t dimension,
                               const std::string &output_path,
                               const HipVsHnswBuildOptions &options)
{
  if (!hipvs_hnsw_enabled() || vectors == nullptr || row_count <= 0 ||
      dimension <= 0 || output_path.empty() || options.max_degree <= 0 ||
      options.ef_construction <= 0) {
    return false;
  }

  try {
    raft::resources resources;
    auto device_dataset = raft::make_device_matrix<float, int64_t>(
        resources, row_count, dimension);
    raft::copy(device_dataset.data_handle(), vectors,
               static_cast<std::size_t>(row_count * dimension),
               raft::resource::get_cuda_stream(resources));
    raft::resource::sync_stream(resources,
                                raft::resource::get_cuda_stream(resources));

    // hipVS builds the GPU graph in CAGRA and converts it to HNSW.  This keeps
    // distance evaluation and graph construction on the Radeon GPU.
    auto cagra_params = cuvs::neighbors::hnsw::to_cagra_params(
        device_dataset.extents(), options.max_degree,
        options.ef_construction);
    auto cagra_index = cuvs::neighbors::cagra::build(
        resources, cagra_params, device_dataset);

    cuvs::neighbors::hnsw::index_params hnsw_params;
    hnsw_params.hierarchy = cuvs::neighbors::hnsw::HnswHierarchy::GPU;
    hnsw_params.ef_construction = options.ef_construction;
    hnsw_params.num_threads = options.num_threads;
    auto hnsw_index = cuvs::neighbors::hnsw::from_cagra(
        resources, hnsw_params, cagra_index);
    cuvs::neighbors::hnsw::serialize(resources, output_path, *hnsw_index);
    return true;
  } catch (...) {
    // Build callers must preserve the existing VSAG fallback on any ROCm or
    // device error.  Do not make a database build fail solely because the
    // optional accelerator is unavailable.
    return false;
  }
}
#else
bool build_hipvs_hnsw_artifact(const float *,
                               int64_t,
                               int64_t,
                               const std::string &,
                               const HipVsHnswBuildOptions &)
{
  return false;
}
#endif

}  // namespace obvsag
}  // namespace common
}  // namespace oceanbase
