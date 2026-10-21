# cida-cli

A standalone Go command-line client for translating and improving text with
your language model. Adapted from [辞达 (Cida)](https://github.com/Xuanwo/cida)
by Xuanwo, using only the Go standard library.

## Download

Download the binary for your platform from
[GitHub Releases](https://github.com/erning/cida-cli/releases).
The first release is `v1.0.0`:

| Platform | Binary |
| --- | --- |
| macOS, Apple Silicon | `cida-darwin_arm64-v1.0.0` |
| macOS, Intel | `cida-darwin_amd64-v1.0.0` |
| Linux, ARM64 | `cida-linux_arm64-v1.0.0` |
| Linux, AMD64 | `cida-linux_amd64-v1.0.0` |

Rename the downloaded file to `cida`, then make it executable:

```sh
chmod +x cida
./cida --version
```

Release binaries do not require Go to be installed. Each release also includes
`SHA256SUMS` and `LICENSE-Cida.txt`. To verify a downloaded binary, download
`SHA256SUMS` into the same directory and check its entry before renaming it.
For example, on macOS with the ARM64 binary:

```sh
grep ' cida-darwin_arm64-v1.0.0$' SHA256SUMS | shasum -a 256 -c -
```

## Build and try

Requires Go 1.26 or newer.

```sh
go build -trimpath -ldflags='-s -w' -o cida .
./cida --help
```

This strips symbols and DWARF debug information while retaining the existing
functionality, including HTTP/2. Use `go build -o cida .` when debugging.

Create `~/.config/cida/config.json` using
[`config.example.json`](config.example.json), then adjust the service and
language settings for your setup. The endpoint is the complete request URL,
including its API path. Use file permissions `600` when storing a key.

```sh
./cida "你好，世界"
./cida --action translate "Hello, world"
./cida -a improve "This are a example."
./cida -a concise "A long paragraph that could be shorter."
```

The CLI reads the configuration on each invocation and never creates or
rewrites it automatically. Its default path is `~/.config/cida/config.json` on
every platform; `XDG_CONFIG_HOME` does not change it. To use another file:

```sh
./cida --config ./config.json "你好"
```

## Actions

`translate` is the default. The model determines the source language: text in
`my_language` is translated into `foreign_language`; text in any other
language is translated into `my_language`. The defaults are `简体中文` and
`English`. Language names can also describe dialects or regional variants.

`improve` revises grammar, clarity, and natural phrasing while keeping the
original language and meaning. Both built-in actions request only the
transformed text and preserve formatting. Their policies are fixed in the
program; `translation_prompt` and `improvement_prompt` are not configuration
fields.

Add custom actions to the `actions` array:

```json
{
  "name": "concise",
  "prompt": "Make the text shorter without losing meaning. Keep the original language. Return only the revised text."
}
```

Invoke it with `./cida -a concise "Text"`. Names are case insensitive and must
not duplicate another action or shadow `translate` or `improve`. Custom
actions follow their own output policy, including any requested format.

Use `--print-prompt` to inspect the assembled system prompt for an action:

```sh
./cida --print-prompt
./cida --print-prompt -a improve
./cida --print-prompt -a concise
```

This prints the action policy, application contract, and runtime parameters
as plain text to stdout. It excludes the source text, ignores text arguments
and standard input, and sends no model request. No input text is required.
The configuration file must exist, but service settings and an API key are
not required in this mode; language defaults and custom actions still apply.

## Input and output

Options must come before text. Multiple text arguments are joined with a
single space. Quote the text or use standard input to preserve newlines:

```sh
cat article.txt | ./cida
./cida -a improve < draft.txt
printf 'First line.\nSecond line.\n' | ./cida -a improve
./cida -- "--help is a command-line option"
./cida "Hello" > translation.txt
```

If text arguments are provided, standard input is ignored. With no arguments
and no redirected input, the CLI prints an error instead of opening an
interactive session. Empty or whitespace-only input is rejected.

Results stream directly to stdout, with a final newline added if needed.
Diagnostics go to stderr. Ctrl+C cancels an active request. A failed stream
can leave partial text on stdout, so scripts should check the exit status.
Streams that end without a completion marker, or that contain a corrupt
event, are treated as failures. Ctrl+C also stops the CLI while it is waiting
for standard input.

| Exit code | Meaning |
| --- | --- |
| `0` | Success |
| `1` | Service, transport, or input/output failure |
| `2` | Invalid arguments, empty input, or configuration error |
| `130` | Interrupted |

`./cida --help` and `./cida --version` work without a configuration file.

## Configuration

Configuration is standard JSON: comments and trailing commas are not accepted.
Unknown fields are rejected.

| Field | Behavior |
| --- | --- |
| `endpoint` | Required full HTTP(S) request URL |
| `format` | Required: `chat-completions`, `responses`, or `anthropic-messages` |
| `model` | Required model identifier |
| `api_key` | Required except for localhost endpoints or `auth: "none"` |
| `my_language` | Default: `简体中文` |
| `foreign_language` | Default: `English` |
| `auth` | `bearer`, `x-api-key`, `api-key`, or `none`; defaults to `x-api-key` for Anthropic and `bearer` otherwise |
| `headers` | Optional extra HTTP headers; override generated headers with the same name |
| `body` | Optional request body overrides; objects merge recursively, arrays and scalars replace, `null` removes a member |
| `actions` | Optional array of objects with `name` and `prompt` |

For example, a `body` of `{"max_tokens": 2048, "thinking": {"type": "disabled"}}`
sets provider-specific options. Anthropic requests default to `max_tokens: 8192`.
Requests use streaming by default; `body` can override this and the client also
accepts complete JSON responses. Responses requests default to `store: false`.

Redirects are followed only within the endpoint's origin (scheme, host, and
port). A redirect to another host, port, or from HTTPS to HTTP fails, so the API
key and text are never forwarded elsewhere; set `endpoint` to the final URL.
Credentials in `headers` (for example `Authorization`) are redacted from
diagnostics like `api_key`.

The client uses standard proxy environment variables. Connection and TLS
handshake timeouts are 30 seconds, response headers have a five-minute timeout,
and active streams have no total duration limit.

## Development

```sh
go test ./...
go vet ./...
go test -race ./...
```

Tests use in-process HTTP fixtures, without a listening port or real API key.
They cover CLI input, action selection, configuration validation, request
formats, streaming and complete responses, cancellation, and error handling.

To embed a version in a local build:

```sh
go build -trimpath -ldflags '-s -w -X main.version=v1.0.0' -o cida .
```

Without version injection, `cida --version` reports `cida dev`.

## Releases

The Git tag is the source of the release version. Tags use `vMAJOR.MINOR.PATCH`,
such as `v1.0.0`, with an optional prerelease suffix such as `v1.1.0-rc.1`.
Prerelease tags produce GitHub prereleases.

To build the four release binaries locally, using the same script as Actions:

```sh
scripts/build-release.sh v1.0.0
```

The files are written to `dist/v1.0.0/`. The script disables cgo for cross
compilation, uses `-trimpath -ldflags='-s -w'`, injects the version, and checks
`--version` on the host's matching binary. It also copies the upstream license
and calculates SHA-256 checksums. Local rebuilds replace generated files in
that version's output directory.

To publish a release after committing and pushing the changes:

```sh
git tag -a v1.0.0 -m 'Release v1.0.0'
git push origin v1.0.0
```

The tag triggers the Release workflow. It runs `go vet` and the tests with race
detection on Linux and macOS, cross-compiles all four targets on Ubuntu, then
creates the GitHub Release and uploads all six assets. The binary names follow
`cida-<os>_<arch>-<version>`. All builds must succeed before publication.

The workflows use the Go 1.26 series, allowing patch updates, and the repository's
`GITHUB_TOKEN`; no personal access token is needed. Rerunning a published version
fails rather than overwriting its assets. Correct a released build with a new
version tag.

## Attribution

The request formats and built-in policies are adapted from
[辞达 (Cida)](https://github.com/Xuanwo/cida) by Xuanwo. The original Cida material is
licensed under [Apache-2.0](licenses/Cida.txt); adapted source files identify
their changes. No GUI assets or third-party Go packages are bundled.
