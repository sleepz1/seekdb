# M1 self-test

Run from the repository root (Go 1.22+ and Bash required):

```sh
./scripts/smoke.sh
```

Observed exit status: 0. Exact observed PASS line:

```text
PASS: build, TCP SQL, vector ranking, SIGKILL snapshot recovery, DELETE/DROP
```

The script builds from source, uses a temporary data directory and ephemeral TCP
port, checks vector ordering, kills the server with SIGKILL, restarts it, and
checks snapshot recovery before DELETE/DROP. Temporary files are cleaned up.
`go test ./...` also passed in the preceding session; there are no Go test files.

Docker build retried successfully (exit status 0):

```sh
sg docker -c 'docker build -t minivectordb:m1 .'
```

Built `minivectordb:m1`, image ID
`sha256:451c6f57e9eb190154036cf684b1160d45dc6ec2f1de0aebfbed6b6a2bf488b8`.
The earlier docker.sock permission error was resolved by `sg docker`.
This verifies the image build; container runtime was not tested in this continuation.
No image was pushed and no formal contest submission was made.

Open validation: official mysqltest pack unknown; MySQL wire protocol TBD;
transaction stubs depth and transaction semantics need further validation.
