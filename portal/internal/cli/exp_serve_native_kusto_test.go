package cli

import "testing"

// TestNativeKustoQueryPrecedence pins the transport precedence rule: an explicit
// --kusto-query-command keeps existing shell-adapter deployments untouched, a
// bare --kusto-endpoint reaches ADX natively, and neither means no transport.
func TestNativeKustoQueryPrecedence(t *testing.T) {
	for _, tc := range []struct {
		name    string
		opts    expServeOptions
		wantNil bool
	}{
		{name: "unconfigured", opts: expServeOptions{}, wantNil: true},
		{name: "query command wins", opts: expServeOptions{kustoQueryCommand: "/adapter/bin/busybox", kustoEndpoint: "https://adx.example.net"}, wantNil: true},
		{name: "bare endpoint goes native", opts: expServeOptions{kustoEndpoint: "https://adx.example.net"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.opts.nativeKustoQuery(); (got == nil) != tc.wantNil {
				t.Fatalf("nativeKustoQuery() nil = %v, want %v", got == nil, tc.wantNil)
			}
		})
	}
}
