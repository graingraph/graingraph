module github.com/graingraph/graingraph

// The go directive selects the toolchain CI and releases build with. 1.26.11
// fixes GO-2026-6859 (net/http MaxBytesReader header race) and
// GO-2026-6809 (crypto/x509 compromised-intermediate handling), both of which
// shipped in this project's own `graingraph-query` HTTP server. It supersedes the
// 1.26.9 bump that closed GO-2026-6617.
go 1.26.11

require (
	github.com/cucumber/godog v0.15.1
	github.com/goccy/go-json v0.10.6
	golang.org/x/text v0.42.0
	modernc.org/sqlite v1.35.0
)

require (
	github.com/cucumber/gherkin/go/v26 v26.2.0 // indirect
	github.com/cucumber/messages/go/v21 v21.0.1 // indirect
	github.com/gofrs/uuid v4.3.1+incompatible // indirect
	github.com/hashicorp/go-immutable-radix v1.3.1 // indirect
	github.com/hashicorp/go-memdb v1.3.4 // indirect
	github.com/hashicorp/golang-lru v0.5.4 // indirect
	github.com/spf13/pflag v1.0.7 // indirect
)

require (
	github.com/dustin/go-humanize v1.0.1 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/mattn/go-isatty v0.0.20 // indirect
	github.com/ncruces/go-strftime v1.0.0 // indirect
	github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec // indirect
	golang.org/x/exp v0.0.0-20230315142452-642cacee5cc0 // indirect
	golang.org/x/sys v0.48.0 // indirect
	modernc.org/libc v1.61.13 // indirect
	modernc.org/mathutil v1.7.1 // indirect
	modernc.org/memory v1.8.2 // indirect
	pgregory.net/rapid v1.3.0
)
