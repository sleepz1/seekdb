#!/usr/bin/env python3
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

"""Linux cloud probe: syntax-check real AI source with its production CMake flags.

Run AFTER `bash build.sh release --init`, in a separate cloud checkout/runner.
No source/header stubs are created. This is not a link or runtime test.
Exit: 0 all syntax checks passed; 1 semantic compiler failure; 2 setup failure;
124 timeout. A Unity action's flags are taken only if it actually includes the TU.
"""

import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import shlex
import signal
import subprocess
import sys
import time


AI_DIR = Path('src/sql/engine/expr/ob_expr_ai')
DEFAULT_SOURCE = AI_DIR / 'ob_expr_ai_rerank.cpp'
INCLUDE = re.compile(r'^\s*#\s*include\s*["<]([^">]+)[">]', re.MULTILINE)
MISSING = re.compile(
    r"fatal error:\s*(?:['\"]([^'\"]+)['\"]\s+file not found|([^:\n]+): No such file or directory)"
)


def resolve(path, directory):
    value = Path(path)
    return (value if value.is_absolute() else directory / value).resolve()


def read_cache(build):
    values = {}
    for line in (build / 'CMakeCache.txt').read_text().splitlines():
        if line and not line.startswith(('#', '//')) and '=' in line and ':' in line.split('=', 1)[0]:
            key, value = line.split('=', 1)
            values[key.split(':', 1)[0]] = value
    return values


def expand_response_files(arguments, directory, depth=0):
    if depth > 8:
        raise ValueError('Compiler response-file nesting exceeds 8')
    expanded = []
    for argument in arguments:
        if argument.startswith('@'):
            response = resolve(argument[1:], directory)
            expanded.extend(expand_response_files(shlex.split(response.read_text()), directory, depth + 1))
        else:
            expanded.append(argument)
    return expanded


def select_entry(database, source):
    direct = []
    containing = []
    for entry in database:
        directory = Path(entry['directory']).resolve()
        input_path = resolve(entry['file'], directory)
        if input_path == source:
            direct.append(entry)
        elif 'Unity' in input_path.parts and input_path.is_file():
            # CMake's generated Unity action names exact production .cpp inputs.
            includes = INCLUDE.findall(input_path.read_text(errors='replace'))
            if any(resolve(include, input_path.parent) == source for include in includes):
                containing.append(entry)
    candidates = direct or containing
    if len(candidates) != 1:
        raise ValueError(f'Expected exactly one production compile action for {source}; found {len(candidates)}')
    return candidates[0]


def syntax_command(entry, source, mode):
    directory = Path(entry['directory']).resolve()
    original_input = resolve(entry['file'], directory)
    argv = entry.get('arguments') or shlex.split(entry['command'])
    argv = expand_response_files(argv, directory)
    if any(argument in {'&&', '||', ';', '|', '>', '<'} for argument in argv):
        raise ValueError('Shell command operators are not supported; expected compiler argv')
    matches = [index for index, argument in enumerate(argv)
               if not argument.startswith('-') and resolve(argument, directory) == original_input]
    if len(matches) != 1:
        raise ValueError(f'Expected exactly one compiler input token; found {len(matches)}')
    input_index = matches[0]
    output = []
    index = 0
    # Remove object/dependency outputs so probing cannot overwrite build artifacts.
    pair_options = {'-o', '-MF', '-MT', '-MQ', '-MJ', '--serialize-diagnostics', '-dependency-file'}
    mode_options = {'-c', '-S', '-E', '-emit-llvm', '-MD', '-MMD', '-MP', '-MG'}
    while index < len(argv):
        argument = argv[index]
        if index == input_index:
            output.append(str(source if mode == 'source' else original_input))
        elif argument in pair_options:
            if index + 1 >= len(argv):
                raise ValueError(f'Missing value for {argument}')
            index += 1
        elif argument in mode_options:
            pass
        elif any(argument.startswith(prefix) and argument != prefix
                 for prefix in ('-MF', '-MT', '-MQ', '-MJ')):
            pass
        elif argument.startswith(('-Wp,-MD,', '-Wp,-MMD,', '--serialize-diagnostics=')):
            pass
        elif argument.startswith('-o') and entry.get('output') and argument[2:] == entry['output']:
            pass
        else:
            output.append(argument)
        index += 1
    output.extend(['-fsyntax-only', '-fno-color-diagnostics'])
    return output, directory, original_input


def run_command(command, directory, destination, timeout, environment=None):
    print(f'COMMAND ({directory}): {shlex.join(command)}', flush=True)
    start = time.monotonic()
    with destination.open('w') as output:
        process = subprocess.Popen(command, cwd=directory, stdout=output,
                                   stderr=subprocess.STDOUT, start_new_session=True, env=environment)
        try:
            returncode = process.wait(timeout=timeout)
        except subprocess.TimeoutExpired:
            try:
                os.killpg(process.pid, signal.SIGTERM)
            except ProcessLookupError:
                pass
            try:
                process.wait(timeout=10)
            except subprocess.TimeoutExpired:
                try:
                    os.killpg(process.pid, signal.SIGKILL)
                except ProcessLookupError:
                    pass
                process.wait()
            returncode = 124
    text = destination.read_text(errors='replace')
    print(text, end='' if text.endswith('\n') else '\n', flush=True)
    return {'returncode': returncode, 'elapsed_seconds': round(time.monotonic() - start, 3),
            'log': str(destination)}, text


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--repo', type=Path, default=Path.cwd())
    parser.add_argument('--build-dir', type=Path, default=Path('build_release'))
    parser.add_argument('--source', action='append', type=Path,
                        help='Actual repo-relative production .cpp; repeat for several')
    parser.add_argument('--all-ai', action='store_true', help='Check every .cpp in the AI expression directory')
    parser.add_argument('--keep-going', action='store_true',
                        help='Collect diagnostics for subsequent sources after a compile/setup failure')
    parser.add_argument('--mode', choices=('source', 'unity'), default='source')
    parser.add_argument('--expect-action-input', type=Path,
                        help='Require exact original compile_commands file path; resolved from its directory')
    parser.add_argument('--prepare', choices=('auto', 'none'), default='auto',
                        help='auto: build existing Rust target only after an actual missing exported header')
    parser.add_argument('--jobs', type=int, default=2)
    parser.add_argument('--timeout', type=int, default=300, help='Seconds per syntax probe')
    parser.add_argument('--prepare-timeout', type=int, default=1200)
    parser.add_argument('--log-dir', type=Path, default=Path('ai-syntax-probe-logs'))
    args = parser.parse_args()
    if sys.platform != 'linux':
        parser.error('Run only on the authorized Linux cloud runner, not the local Windows machine')
    if min(args.jobs, args.timeout, args.prepare_timeout) < 1:
        parser.error('Jobs and timeouts must be positive')
    repo = args.repo.resolve()
    build = resolve(args.build_dir, repo)
    logs = resolve(args.log_dir, repo)
    logs.mkdir(parents=True, exist_ok=True)
    summary = {'mode': args.mode, 'repo': str(repo), 'build': str(build),
               'checks': [], 'prerequisites': [], 'limitation': 'Syntax only; no link/runtime claim'}
    overall = 0
    prepared_rust = False
    try:
        cache = read_cache(build)
        if resolve(cache['CMAKE_HOME_DIRECTORY'], build) != repo:
            raise ValueError('CMake cache belongs to a different source checkout')
        database = json.loads((build / 'compile_commands.json').read_text())
        exports = json.loads((repo / 'rust/exports.json').read_text())
        exported_headers = {header for item in exports.values() for header in item['headers']}
        cmake = cache.get('CMAKE_COMMAND', 'cmake')
        sources = args.source or ([p.relative_to(repo) for p in sorted((repo / AI_DIR).glob('*.cpp'))]
                                  if args.all_ai else [DEFAULT_SOURCE])
        for number, relative in enumerate(sources, 1):
            check_code = 0
            source = resolve(relative, repo)
            if not source.is_relative_to(repo / 'src') or source.suffix != '.cpp' or not source.is_file():
                raise ValueError(f'Not an existing production src/*.cpp: {source}')
            entry = select_entry(database, source)
            command, directory, original = syntax_command(entry, source, args.mode)
            if args.expect_action_input and resolve(args.expect_action_input, directory) != original:
                raise ValueError(f'Original action input mismatch: {original}')
            record = {'source': str(source.relative_to(repo)),
                      'source_sha256': hashlib.sha256(source.read_bytes()).hexdigest(),
                      'production_action_input': str(original), 'command': command, 'attempts': []}
            summary['checks'].append(record)
            for attempt in (1, 2):
                result, diagnostic = run_command(command, directory,
                    logs / f'{number:02d}-{source.stem}-{attempt}.log', args.timeout)
                record['attempts'].append(result)
                missing = {next(value for value in match if value) for match in MISSING.findall(diagnostic)}
                result['missing_headers'] = sorted(missing)
                if (attempt == 1 and args.prepare == 'auto' and not prepared_rust
                        and missing.intersection(exported_headers)):
                    prepared_rust = True
                    preparation, _ = run_command(
                        [cmake, '--build', str(build), '--target', 'rust_staticlibs_build',
                         '--parallel', str(args.jobs)], repo, logs / 'prepare-rust.log', args.prepare_timeout,
                        {**os.environ, 'CARGO_BUILD_JOBS': str(args.jobs)})
                    summary['prerequisites'].append(preparation)
                    if preparation['returncode'] != 0:
                        check_code = 124 if preparation['returncode'] == 124 else 2
                        record['status'] = 'prerequisite_failed'
                        break
                    continue
                if result['returncode'] == 0:
                    record['status'] = 'syntax_passed'
                elif result['returncode'] == 124:
                    record['status'] = 'timeout'
                    check_code = 124
                elif missing:
                    record['status'] = 'missing_prerequisite'
                    check_code = 2
                else:
                    record['status'] = 'compiler_failed'
                    check_code = 1
                break
            severity = {0: 0, 1: 1, 2: 2, 124: 3}
            overall = max((overall, check_code), key=lambda code: severity[code])
            if check_code and not args.keep_going:
                break
    except (OSError, KeyError, ValueError) as error:
        summary['setup_error'] = str(error)
        overall = 2
        print(f'SETUP ERROR: {error}', file=sys.stderr)
    summary['exit_code'] = overall
    (logs / 'summary.json').write_text(json.dumps(summary, indent=2) + '\n')
    print(f'Probe evidence: {logs / "summary.json"}', flush=True)
    return overall


if __name__ == '__main__':
    raise SystemExit(main())
