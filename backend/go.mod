module github.com/yugabyte/meko/backend

go 1.26.0

require (
	github.com/BurntSushi/toml v1.6.0
	github.com/google/uuid v1.6.0
	github.com/labstack/echo/v4 v4.16.0
	github.com/mattn/go-sqlite3 v1.14.52
	github.com/modelcontextprotocol/go-sdk v1.8.0
	golang.org/x/sys v0.49.0
)

require (
	github.com/google/jsonschema-go v0.4.3 // indirect
	github.com/labstack/gommon v0.5.0 // indirect
	github.com/mattn/go-colorable v0.1.15 // indirect
	github.com/mattn/go-isatty v0.0.22 // indirect
	github.com/segmentio/asm v1.1.3 // indirect
	github.com/segmentio/encoding v0.5.4 // indirect
	github.com/valyala/bytebufferpool v1.0.0 // indirect
	github.com/valyala/fasttemplate v1.2.2 // indirect
	github.com/yosida95/uritemplate/v3 v3.0.2 // indirect
	golang.org/x/crypto v0.53.0 // indirect
	golang.org/x/net v0.56.0 // indirect
	golang.org/x/oauth2 v0.35.0 // indirect
	golang.org/x/sync v0.22.0 // indirect
	golang.org/x/text v0.40.0 // indirect
	golang.org/x/time v0.15.0 // indirect
)

replace github.com/mattn/go-sqlite3 => github.com/yugabyte/go-sqlite3 v1.14.52-yb.1
