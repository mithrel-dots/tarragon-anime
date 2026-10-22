set positional-arguments

# Live provider checks with a Markdown results table.
# Example: just smoke-providers -output results.md
smoke-providers *args:
    @go run ./cmd/provider-smoke "$@"
