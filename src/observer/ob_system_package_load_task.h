/*
 * Copyright (c) 2025 OceanBase.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */
#ifndef _OCEANBASE_ROOTSERVER_OB_SYSTEM_PACKAGE_LOAD_TASK_H_
#define _OCEANBASE_ROOTSERVER_OB_SYSTEM_PACKAGE_LOAD_TASK_H_ 1

#include "common/ob_timeout_ctx.h"
#include "lib/task/ob_timer.h"
#include "lib/utility/ob_macro_utils.h"
#include "share/ob_define.h"

namespace oceanbase
{
namespace rootserver
{
class ObSystemPackageLoadTask : public common::ObTimerTask
{
public:
  ObSystemPackageLoadTask();
  virtual ~ObSystemPackageLoadTask() {}

  int init();
  bool is_inited() const { return inited_; }
  int start(common::ObTimer &timer);
  void stop(common::ObTimer &timer);
  void destroy();

  virtual void runTimerTask() override;

  static int wait_system_package_ready(const common::ObTimeoutCtx &ctx);

private:
  int load_system_package_();
  bool wait_service_started_();
  static void yield_to_user_ddl_();
  static thread_local int64_t tl_yield_deadline_;

private:
  bool inited_;
  int64_t fail_count_;
  static const int64_t SCHEDULE_INTERVAL_US = 3 * 1000 * 1000L; // 3s
  // pacing of the asynchronous load, see yield_to_user_ddl_()
  static const int64_t DDL_QUIET_US = 500L * 1000L; // 500ms
  static const int64_t DDL_CHECK_INTERVAL_US = 20L * 1000L; // 20ms
  static const int64_t MAX_YIELD_US = 60L * 1000L * 1000L; // 60s per load

private:
  DISALLOW_COPY_AND_ASSIGN(ObSystemPackageLoadTask);
};
}
}

#endif // _OCEANBASE_ROOTSERVER_OB_SYSTEM_PACKAGE_LOAD_TASK_H_
