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
#define USING_LOG_PREFIX RS

#include "observer/ob_system_package_load_task.h"
#include "pl/ob_pl_package_manager.h"
#include "rootserver/ob_admin_job_table_operator.h"
#include "share/ob_server_struct.h"
#include "share/rc/ob_server_runtime.h"
#include "config_bridge.h"
#include "lib/time/ob_time_utility.h"
#include "observer/ob_server.h"
#include "rootserver/ddl_task/ob_ddl_scheduler.h"

namespace oceanbase
{
namespace rootserver
{
thread_local int64_t ObSystemPackageLoadTask::tl_yield_deadline_ = 0;


const int64_t ObSystemPackageLoadTask::SCHEDULE_INTERVAL_US;

ObSystemPackageLoadTask::ObSystemPackageLoadTask()
  : inited_(false),
    fail_count_(0)
{
}

int ObSystemPackageLoadTask::init()
{
  int ret = OB_SUCCESS;
  if (OB_UNLIKELY(inited_)) {
    ret = OB_INIT_TWICE;
  } else {
    inited_ = true;
    fail_count_ = 0;
  }
  return ret;
}

int ObSystemPackageLoadTask::start(common::ObTimer &timer)
{
  int ret = OB_SUCCESS;
  const bool did_repeat = true;
  if (OB_UNLIKELY(!inited_)) {
    ret = OB_NOT_INIT;
  } else {
    if (timer.task_exist(*this)) {
      // ignore duplicate schedule
    } else if (OB_FAIL(timer.schedule(*this, SCHEDULE_INTERVAL_US, did_repeat,
                                      config::_enable_async_load_sys_package()/*immediate*/))) {
    } else {
      LOG_INFO("finish schedule timer task", K(SCHEDULE_INTERVAL_US));
    }
  }
  return ret;
}

void ObSystemPackageLoadTask::stop(common::ObTimer &timer)
{
  if (timer.inited()) {
    int ret = OB_SUCCESS;
    if (OB_FAIL(timer.cancel_task(*this))) {
    }
    if (OB_FAIL(timer.wait_task(*this))) {
    }
  }
}

void ObSystemPackageLoadTask::destroy()
{
  inited_ = false;
  fail_count_ = 0;
}

int ObSystemPackageLoadTask::load_system_package_()
{
  int ret = OB_SUCCESS;
  ObMySQLProxy *sql_proxy = GCTX.sql_proxy_;
  int64_t job_id = OB_INVALID_ID;
  int64_t job_count = 0;
  bool need_load = false;
  if (OB_ISNULL(sql_proxy)) {
    ret = OB_ERR_UNEXPECTED;
  } else if (OB_FAIL(GET_ADMIN_JOB_COUNT(LOAD_MYSQL_SYS_PACKAGE, job_count))) {
  } else if (0 == job_count) {
    // job not exists, try insert inprogress job. With asynchronous loading (this round runs
    // only after the observer started service, see wait_service_started_()) load in this round
    // instead of waiting for the next timer round, so that the loading work is done as early
    // as possible after startup instead of overlapping the first user workload.
    if (OB_FAIL(ADMIN_JOB_CREATE_WITH_RET(job_id, JOB_TYPE_LOAD_MYSQL_SYS_PACKAGE))) {
    } else {
      need_load = config::_enable_async_load_sys_package();
    }
  } else if (OB_FAIL(ADMIN_JOB_FIND(LOAD_MYSQL_SYS_PACKAGE, job_id))) {
    if (ret == OB_ENTRY_NOT_EXIST) {
      ret = OB_SUCCESS;
      GCTX.sys_package_ready_ = true;
      LOG_INFO("find a success job or job_id not exist", KR(ret), K(job_id));
    } else {
    }
  } else {
    need_load = true;
  }
  if (OB_SUCC(ret) && need_load) {
    tl_yield_deadline_ = ObTimeUtility::current_time() + MAX_YIELD_US;
    if (OB_FAIL(pl::ObPLPackageManager::load_all_common_sys_package(
                    *sql_proxy,
                    false/*from_file*/,
                    config::_enable_async_load_sys_package() ? yield_to_user_ddl_ : nullptr))) {
    } else if (OB_FAIL(ADMIN_JOB_COMPLETE(job_id, 0/*result_code*/))) {
    } else {
      GCTX.sys_package_ready_ = true;
    }
  }
  return ret;
}

void ObSystemPackageLoadTask::runTimerTask()
{
  int ret = OB_SUCCESS;
  if (OB_UNLIKELY(!inited_)) {
    ret = OB_NOT_INIT;
  } else if (!share::server_is_write_enabled()) {
    LOG_INFO("read-only server skips loading sys package");
  } else if (GCTX.sys_package_ready_) {
    LOG_INFO("sys package already loaded");
  } else if (!wait_service_started_()) {
    LOG_INFO("observer has not started service yet, load sys package in the next round");
  } else if (OB_FAIL(load_system_package_())) {
    fail_count_++;
    if (fail_count_ >= 5) {
      LOG_ERROR("failed to execute system package load task, will retry", KR(ret), K_(fail_count));
    } else {
    }
  } else {
    fail_count_ = 0;
    LOG_INFO("finish loading sys packages");
  }
}

void ObSystemPackageLoadTask::yield_to_user_ddl_()
{
  // Asynchronous loading is background work nobody waits for (synchronous loading, where
  // bootstrap waits, is not paced). Creating the packages costs seconds of CPU and a stream
  // of DDL/schema refreshes, which slows down user DDL tasks that run at the same time (e.g.
  // the first full text index builds after startup take up to 2x longer). So before each
  // package wait until no long running DDL task has been scheduled for DDL_QUIET_US; the
  // waiting is bounded by MAX_YIELD_US per load, and stops when the server stops.
  rootserver::ObDDLScheduler *ddl_scheduler = observer::ObServer::get_instance().ddl_scheduler();
  int64_t quiet_begin = -1;
  int64_t yield_cnt = 0;
  while (nullptr != ddl_scheduler) {
    const int64_t now = ObTimeUtility::current_time();
    if (now >= tl_yield_deadline_ || observer::ObServer::get_instance().is_stopped()) {
      break;
    } else if (ddl_scheduler->get_task_cnt() > 0) {
      quiet_begin = -1;
    } else if (quiet_begin < 0) {
      quiet_begin = now;
    } else if (now - quiet_begin >= DDL_QUIET_US) {
      break;
    }
    ++yield_cnt;
    ob_usleep(DDL_CHECK_INTERVAL_US);
  }
  if (yield_cnt * DDL_CHECK_INTERVAL_US > DDL_QUIET_US) {
    LOG_INFO("sys package load yielded to user ddl", "wait_us", yield_cnt * DDL_CHECK_INTERVAL_US);
  }
}

bool ObSystemPackageLoadTask::wait_service_started_()
{
  // With asynchronous loading the first round runs right after activation (see start()),
  // which may still be inside bootstrap; nobody waits for the packages there, so do not run
  // the loading DDL concurrently with bootstrap: wait (bounded by one schedule interval) until
  // the observer starts service and load right then. With synchronous loading bootstrap
  // itself waits for the packages and the rounds are scheduled exactly as before.
  bool started = true;
  if (config::_enable_async_load_sys_package()) {
    const int64_t check_interval_us = 20L * 1000L; // 20ms
    const int64_t deadline = ObTimeUtility::current_time() + SCHEDULE_INTERVAL_US;
    started = GCTX.start_service_time_ > 0;
    while (!started && ObTimeUtility::current_time() < deadline) {
      ob_usleep(check_interval_us);
      started = GCTX.start_service_time_ > 0;
    }
  }
  return started;
}

int ObSystemPackageLoadTask::wait_system_package_ready(const common::ObTimeoutCtx &ctx)
{
  int ret = OB_SUCCESS;
  const int64_t retry_interval_us = 500l * 1000l;
  int64_t job_id = OB_INVALID_ID;
  bool finish = false;
  int64_t inprogress_job_count = 0;
  while (OB_SUCC(ret) && !finish) {
    int tmp_ret = OB_SUCCESS;
    if (ctx.is_timeouted()) {
      ret = OB_TIMEOUT;
    } else {
      inprogress_job_count = 0;
      if (OB_ENTRY_NOT_EXIST != (tmp_ret = ADMIN_JOB_FIND(LOAD_MYSQL_SYS_PACKAGE, job_id))) {
        inprogress_job_count++;
      }
      if (inprogress_job_count == 0) {
        finish = true;
      } else {
        ob_usleep(retry_interval_us);
      }
    }
  }
  return ret;
}

} // namespace rootserver
} // namespace oceanbase
