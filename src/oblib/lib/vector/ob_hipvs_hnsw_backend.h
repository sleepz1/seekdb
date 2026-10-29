/*
 * Optional AMD hipVS HNSW builder for the AE-01 competition path.
 *
 * The normal VSAG path remains the default.  This adapter is compiled only
 * when OB_ENABLE_HIPVS is enabled and is selected explicitly with
 * OB_HIPVS_HNSW=1.  It emits the hipVS hnswlib-compatible artifact requested
 * by the caller; callers can keep using the VSAG artifact when hipVS is not
 * available.
 */
#ifndef OB_HIPVS_HNSW_BACKEND_H
#define OB_HIPVS_HNSW_BACKEND_H

#include <cstdint>
#include <string>

namespace oceanbase {
namespace common {
namespace obvsag {

struct HipVsHnswBuildOptions {
  int max_degree = 32;
  int ef_construction = 200;
  int num_threads = 0;
};

// Returns true only when the operator explicitly opted in to the GPU path.
bool hipvs_hnsw_enabled();

// Build a FP32/L2 HNSW artifact with hipVS' GPU CAGRA + HNSW conversion.
// The output is compatible with hipVS' hnsw::deserialize API.  The function
// returns false when hipVS is not compiled in, the input is invalid, or the
// GPU build fails.  The caller must fall back to VSAG in that case.
bool build_hipvs_hnsw_artifact(const float *vectors,
                               int64_t row_count,
                               int64_t dimension,
                               const std::string &output_path,
                               const HipVsHnswBuildOptions &options);

}  // namespace obvsag
}  // namespace common
}  // namespace oceanbase

#endif  // OB_HIPVS_HNSW_BACKEND_H
