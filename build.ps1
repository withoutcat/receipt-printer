$ErrorActionPreference = "Stop"

go mod tidy
go build -ldflags "-s -w" -o print_receipt.exe .

