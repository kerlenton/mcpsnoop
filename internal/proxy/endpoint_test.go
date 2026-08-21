package proxy

import (
	"strings"
	"testing"
)

// TestEndpointForLogKeepsIdentityAndDropsCredentials pins what an HTTP session
// log is allowed to say about its endpoint. Redaction is off unless the user
// asks for it, so anything this keeps is kept for everyone.
func TestEndpointForLogKeepsIdentityAndDropsCredentials(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		want string
	}{
		{"plain", "https://api.example.com/mcp", "https://api.example.com/mcp"},
		{"port and path survive", "http://localhost:3000/v1/mcp", "http://localhost:3000/v1/mcp"},
		{"userinfo leaves a marker", "https://user:pw@api.example.com/mcp", "https://[stripped]@api.example.com/mcp"},
		{"user only is still a credential", "https://sk-live-abc@api.example.com/mcp", "https://[stripped]@api.example.com/mcp"},
		{"query keys stay, values go", "https://h/mcp?api_key=sk-live-abc&tenant=acme", "https://h/mcp?api_key=[stripped]&tenant=[stripped]"},
		{"an empty value has nothing to remove", "https://h/mcp?debug=", "https://h/mcp?debug="},
		{"a bare key has no value", "https://h/mcp?debug", "https://h/mcp?debug"},
		{"the fragment goes", "https://h/mcp#token", "https://h/mcp"},
		{"everything at once", "https://u:p@h:8443/mcp?k=v#f", "https://[stripped]@h:8443/mcp?k=[stripped]"},
		{"an escaped path stays escaped", "https://h/a%20b/mcp", "https://h/a%20b/mcp"},
		{"no host is not an endpoint", "/mcp", ""},
		{"empty is empty", "", ""},
		{"whitespace only", "   ", ""},
		{"unparseable records nothing", "http://[::1", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := EndpointForLog(tc.in); got != tc.want {
				t.Fatalf("EndpointForLog(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestEndpointForLogNeverEchoesASecret is the property the table above is a
// sample of. A value is a secret whatever it is spelled like, so no input that
// carries one may come back out, including one shaped like the marker itself.
func TestEndpointForLogNeverEchoesASecret(t *testing.T) {
	const secret = "sk-live-do-not-log-me"
	for _, in := range []string{
		"https://" + secret + "@h/mcp",
		"https://u:" + secret + "@h/mcp",
		"https://h/mcp?token=" + secret,
		"https://h/mcp?a=1&token=" + secret + "&b=2",
		"https://h/mcp#" + secret,
		"https://h/mcp?x=[stripped]&token=" + secret,
	} {
		if got := EndpointForLog(in); strings.Contains(got, secret) {
			t.Fatalf("EndpointForLog(%q) = %q, which still carries the secret", in, got)
		}
	}
}
