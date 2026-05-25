$ErrorActionPreference = "Stop"

go mod tidy
go build -ldflags "-s -w" -o receipt-printer.exe .

