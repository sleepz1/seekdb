#!/usr/bin/env bash
# Copyright (c) 2026 OceanBase.
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
# http://www.apache.org/licenses/LICENSE-2.0
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

# Disposable Ubuntu Actions runner only. Run unchanged tests against started seekdb.
set -euo pipefail
[[ ${GITHUB_ACTIONS:-} == true && ${RUNNER_OS:-} == Linux ]] || {
  echo 'This wrapper requires a disposable Linux GitHub Actions runner.' >&2; exit 2;
}
: "${GITHUB_WORKSPACE:?}" "${RUNNER_TEMP:?}"
REPO_ROOT=${REPO_ROOT:-$GITHUB_WORKSPACE}
SEEKDB_BASE_DIR=${SEEKDB_BASE_DIR:-$RUNNER_TEMP/sdb04-instance}
MYSQLTEST=${MYSQLTEST:-$REPO_ROOT/deps/3rd/u01/obclient/bin/mysqltest}
OBCLIENT=${OBCLIENT:-$REPO_ROOT/deps/3rd/u01/obclient/bin/obclient}
ARTIFACT_DIR=${ARTIFACT_DIR:-$REPO_ROOT/official-mysqltest-evidence}
PORT=${PORT:-2881}
[[ $REPO_ROOT == /* && $SEEKDB_BASE_DIR == /* && $ARTIFACT_DIR == /* ]]
[[ -x $MYSQLTEST && -x $OBCLIENT && -d $SEEKDB_BASE_DIR && $PORT =~ ^[0-9]+$ ]]
mkdir -p "$ARTIFACT_DIR"
ARTIFACT_DIR=$(realpath "$ARTIFACT_DIR")
scratch=$(mktemp -d "$RUNNER_TEMP/sdb04-official.XXXXXXXX")
marker="sdb04-official-$$-$RANDOM"
ssh_dir="$HOME/.ssh"
auth_keys="$ssh_dir/authorized_keys"
[[ ! -L $ssh_dir && ! -L $auth_keys ]]
auth_existed=0; [[ ! -e $auth_keys ]] || auth_existed=1
mock_pid=''; key_installed=0
cleanup() {
  exit_status=$?
  if [[ $exit_status != 0 ]]; then
    for diagnostic in fixture-sql-init.log fixture-ssh-check.log fixture-sshd.log; do
      if [[ -r $ARTIFACT_DIR/$diagnostic ]]; then
        printf 'Setup diagnostic: %s\n' "$diagnostic"
        tail -n 30 "$ARTIFACT_DIR/$diagnostic"
      fi
    done
  fi
  [[ -z $mock_pid ]] || { kill "$mock_pid" 2>/dev/null || true; wait "$mock_pid" 2>/dev/null || true; }
  if [[ -f $scratch/sshd.pid ]]; then
    ssh_pid=$(cat "$scratch/sshd.pid")
    [[ ! $ssh_pid =~ ^[0-9]+$ ]] || sudo kill "$ssh_pid" 2>/dev/null || true
  fi
  if [[ $key_installed == 1 && -f $auth_keys ]]; then
    sed -i "/ $marker\$/d" "$auth_keys"
    [[ $auth_existed == 1 || -s $auth_keys ]] || rm -f "$auth_keys"
  fi
  # mktemp created this exact directory beneath RUNNER_TEMP.
  [[ $scratch == "$RUNNER_TEMP"/sdb04-official.* ]] && rm -rf -- "$scratch"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

"$OBCLIENT" -h127.0.0.1 -P"$PORT" -uroot -A -e \
  "CREATE DATABASE IF NOT EXISTS test; CREATE USER IF NOT EXISTS 'admin' IDENTIFIED BY 'admin'; GRANT ALL ON test.* TO 'admin';" \
  > "$ARTIFACT_DIR/fixture-sql-init.log" 2>&1
fixture_host=$("$OBCLIENT" -h127.0.0.1 -P"$PORT" -uroot -A -N -s -e 'SELECT host_ip()')
# Validate the SQL-selected address is assigned to this runner; do not print it.
python3 - "$fixture_host" <<'PY'
import ipaddress, json, subprocess, sys
target = ipaddress.ip_address(sys.argv[1].strip())
addresses = json.loads(subprocess.check_output(["ip", "-j", "address"], text=True))
assert target.version == 4 and any(str(target) == a["local"] for i in addresses for a in i["addr_info"]), "fixture host must be local IPv4"
PY
if [[ ! -x /usr/sbin/sshd ]]; then
  sudo apt-get update -qq > "$ARTIFACT_DIR/openssh-install.log" 2>&1
  sudo apt-get install -y -qq openssh-server >> "$ARTIFACT_DIR/openssh-install.log" 2>&1
fi
sudo mkdir -p /run/sshd
sudo ssh-keygen -A > "$ARTIFACT_DIR/ssh-hostkey-setup.log" 2>&1
mkdir -p "$ssh_dir" "$scratch/bin"
chmod 700 "$ssh_dir" "$scratch"
touch "$auth_keys"; chmod 600 "$auth_keys"
ssh-keygen -q -t ed25519 -N '' -C "$marker" -f "$scratch/client_key"
printf '\n' >> "$auth_keys"
key_installed=1
cat "$scratch/client_key.pub" >> "$auth_keys"
runner_user=$(id -un)
cat > "$scratch/sshd_config" <<EOF
Port 22222
ListenAddress 127.0.0.1
HostKey /etc/ssh/ssh_host_ed25519_key
PidFile "$scratch/sshd.pid"
AuthorizedKeysFile "$auth_keys"
AllowUsers $runner_user
AuthenticationMethods publickey
PubkeyAuthentication yes
PasswordAuthentication no
KbdInteractiveAuthentication no
UsePAM no
PermitRootLogin no
AllowAgentForwarding no
AllowTcpForwarding no
X11Forwarding no
PermitTTY no
LogLevel ERROR
Subsystem sftp internal-sftp
EOF
[[ $fixture_host == 127.0.0.1 ]] || printf 'ListenAddress %s\n' "$fixture_host" >> "$scratch/sshd_config"
for address in "$fixture_host" 127.0.0.1; do
  printf '[%s]:22222 ' "$address" >> "$scratch/known_hosts"
  cat /etc/ssh/ssh_host_ed25519_key.pub >> "$scratch/known_hosts"
done
cat > "$scratch/client_config" <<EOF
Host $fixture_host 127.0.0.1
    User $runner_user
    Port 22222
    IdentityFile "$scratch/client_key"
    IdentitiesOnly yes
    IdentityAgent none
    UserKnownHostsFile "$scratch/known_hosts"
    GlobalKnownHostsFile /dev/null
    StrictHostKeyChecking yes
    BatchMode yes
    ConnectTimeout 5
EOF
export SDB04_SSH_CONFIG="$scratch/client_config"
for tool in ssh scp; do
  printf '#!/usr/bin/env bash\nexec /usr/bin/%s -F "$SDB04_SSH_CONFIG" "$@"\n' "$tool" > "$scratch/bin/$tool"
  chmod 700 "$scratch/bin/$tool"
done
sudo /usr/sbin/sshd -t -f "$scratch/sshd_config"
# Create the log as the runner so artifact collection can read it after sudo sshd.
touch "$ARTIFACT_DIR/fixture-sshd.log"
chmod 600 "$ARTIFACT_DIR/fixture-sshd.log"
sudo /usr/sbin/sshd -f "$scratch/sshd_config" -E "$ARTIFACT_DIR/fixture-sshd.log"
export PATH="$scratch/bin:$(dirname "$OBCLIENT"):$PATH"
ssh "$fixture_host" true > "$ARTIFACT_DIR/fixture-ssh-check.log" 2>&1
export OBMYSQL_MS0=127.0.0.1 OBMYSQL_MS0_DEV=127.0.0.1 OBMYSQL_PORT="$PORT"
export OBMYSQL_USR=admin OBMYSQL_PWD=admin OBSERVER_DIR="$SEEKDB_BASE_DIR" IS_BUSINESS=0 TENANT=mysql
cd "$REPO_ROOT"
python3 - <<'PY'
import socket
with socket.socket() as probe:
    probe.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
    probe.bind(("0.0.0.0", 8000))
PY
python3 -u -c 'import runpy; from http.server import ThreadingHTTPServer; fixture=runpy.run_path("unittest/sql/engine/expr/ai_function_contest_check.py"); ThreadingHTTPServer(("0.0.0.0",8000),fixture["MockHandler"]).serve_forever()' \
  > "$ARTIFACT_DIR/official-ai-mock.log" 2>&1 &
mock_pid=$!
python3 - "$mock_pid" <<'PY'
import os, socket, sys, time
for attempt in range(50):
    os.kill(int(sys.argv[1]), 0)
    try:
        with socket.create_connection(("127.0.0.1", 8000), timeout=0.2): break
    except OSError: time.sleep(0.1)
else: raise SystemExit("official mock did not become ready")
PY
status=0
printf 'suite\tcase\texit_code\n' > "$ARTIFACT_DIR/results.tsv"
for entry in ai_function:ai_prompt ai_function:ai_function_enhancement file_sql:file_sql_basic; do
  suite=${entry%:*}; case_name=${entry#*:}
  suite_dir="$REPO_ROOT/tools/deploy/mysql_test/test_suite/$suite"
  case_log="$ARTIFACT_DIR/$case_name"; mkdir -p "$case_log/tmp"
  sha256sum "$suite_dir/t/$case_name.test" "$suite_dir/r/mysql/$case_name.result" > "$case_log/source.sha256"
  rc=0
  ( cd "$REPO_ROOT/tools/deploy"
    timeout --signal=TERM 300s "$MYSQLTEST" --host=127.0.0.1 --port="$PORT" --user=root --database=test \
      --tmpdir="$case_log/tmp" --logdir="$case_log" --silent \
      --test-file="$suite_dir/t/$case_name.test" --result-file="$suite_dir/r/mysql/$case_name.result" \
      --timer-file="$case_log/timer" --tail-lines=20
  ) > "$case_log/console.log" 2>&1 || rc=$?
  sha256sum --check "$case_log/source.sha256" > "$case_log/source-integrity.log" 2>&1 || rc=1
  printf '%s\t%s\t%s\n' "$suite" "$case_name" "$rc" >> "$ARTIFACT_DIR/results.tsv"
  printf 'mysqltest %s: exit %s\n' "$case_name" "$rc"
  [[ $rc == 0 ]] || status=1
done
exit "$status"

