#!/bin/bash

# Run only in remote environment
if [ "$CLAUDE_CODE_REMOTE" != "true" ]; then
  exit 0
fi

# Install tools
go install github.com/golangci/golangci-lint/cmd/golangci-lint@latest

# Build the project
go build ./...
exit 0
