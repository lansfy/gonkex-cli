# Gonkex-cli: testing automation tool

## Using the CLI

To test a service located on a remote host, use gonkex as a console util.

Usage:

`./gonkex -host <...> -tests <...> [-db-type <...> -db-dsn <...> -fixtures <...>] [-allure] [-v]`

- `-host <...>` target system hostname (host:port)
- `-tests <...>` path to tests file or directory
- `-db-type <...>` - database type (available options: postgres, mysql, sqlite, aerospike, redis)
- `-db-dsn <...>` DSN for the database (WARNING: tables mentioned in fixtures will be truncated!)
  * when using Aerospike - connection URL in a form of `host:port/namespace`
  * when using Redis - connection address, for example `redis://user:password@localhost:6789/1?dial_timeout=1&db=1&read_timeout=6s&max_retries=2`
- `-fixtures <...>` path to fixtures directory
- `-env-file <...>` path to env-file
- `-mocks <...>` comma separated list of registered mocks
- `-mocks-prefix <...>` use specified prefix when register environment variables (default "GONKEX_MOCK_")
- `-mocks-defaults <...>` mock values applied after mock creation
- `-pre-test-cmd <...>` program to run before start the tests
- `-pre-test-wait <...>` delay before start the tests
- `-allure` generate an Allure-report
- `-v` verbose output
