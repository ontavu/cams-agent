#!/usr/bin/env bash
# Every check this repository has to pass. Run identically by
# .github/workflows/ci.yml and by a person, from anywhere in the clone.

set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/.."

log() { printf '\n=== %s\n' "$*"; }
die() { printf 'error: %s\n' "$*" >&2; exit 1; }

for tool in go gofmt git; do
    command -v "$tool" >/dev/null || die "$tool is not on PATH"
done

# Never a workspace: this module is checked against the tagged versions of
# cams-api and cams-go-utils its go.mod requires, as its CI and its image build.
export GOWORK=off

scratch="$(mktemp -d)"
trap 'rm -rf "$scratch"' EXIT

# Compared before and after rather than required clean, so that the checks can
# run on a change that is not committed yet.
tree_state() { git status --porcelain | LC_ALL=C sort; git diff; }
tree_before="$(tree_state)"

log "toolchain"
go version

log "go mod download"
go mod download

# gofmt -l exits 0 whether or not it found anything.
log "gofmt"
unformatted="$(gofmt -l .)"
[ -z "$unformatted" ] || { gofmt -d .; die "these files are not gofmt'd: $unformatted"; }

log "go vet"
go vet ./...

# Into the scratch directory: `go build ./...` would drop nothing, and
# `go install` would write outside the clone.
log "go build ./bin/..."
go build -o "$scratch/bin/" ./bin/...
"$scratch/bin/cams-agent" --help > /dev/null
"$scratch/bin/cams-cli" --help > /dev/null

# An agent package is imported by main and nothing else; see AGENTS.md. A
# controller package may be named by an agent, an agent package never.
log "agent packages are imported by main alone"
violations=""
for agent in camagent lanagent upagent; do
    violations+="$(go list -f "{{if ne .Name \"main\"}}{{range .Imports}}{{if eq . \"github.com/ontavu/cams-agent/$agent\"}}{{\$.ImportPath}} imports $agent
{{end}}{{end}}{{end}}" ./...)"
done
[ -z "$violations" ] || die "an agent package is imported outside main:
$violations"

# -race because the whole design is concurrent: three agent kinds, a message
# bus and a media path. A run without it proves very little here.
log "go test -race"
go test -race -count=1 -timeout 15m ./...

# A stale go.mod or go.sum is a build that works on one machine and not the next.
log "go.mod is tidy"
cp go.mod go.sum "$scratch/"
go mod tidy
cmp -s go.mod "$scratch/go.mod" && cmp -s go.sum "$scratch/go.sum" \
    || die "go.mod or go.sum is not tidy, run 'go mod tidy'"

log "the working tree is unchanged"
if [ "$(tree_state)" != "$tree_before" ]; then
    diff <(printf '%s\n' "$tree_before") <(tree_state) || true
    die "the checks changed the working tree"
fi

log "OK"
