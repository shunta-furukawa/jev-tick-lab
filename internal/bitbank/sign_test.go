package bitbank

import "testing"

// The vendor publishes worked examples with their expected signatures. Testing
// against those is the whole reason this can be trusted before a real key ever
// touches it: a signing bug otherwise shows up as an opaque 20003 from the
// exchange, at the worst possible moment.
//
// Vectors from bitbank-api-docs/rest-api.md, "ACCESS-SIGNATURE / Sample".
const (
	docSecret = "hoge"
	docTime   = int64(1721121776490)
	docWindow = int64(1000)
	docBody   = `{"pair": "xrp_jpy", "price": "20", "amount": "1","side": "buy", "type": "limit"}`
)

func TestSignatureMatchesTheVendorsPublishedVectors(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		got  string
		want string
	}{
		{
			"time-window GET",
			signTimeWindow(docSecret, docTime, docWindow, "/v1/user/assets"),
			"9ec5745960d05573c8fb047cdd9191bd0c6ede26f07700bb40ecf1a3920abae8",
		},
		{
			"time-window POST",
			signTimeWindow(docSecret, docTime, docWindow, docBody),
			"7868665738ae3f8a796224e0413c1351ddd7ec2af121db12815c0a5b74b8764c",
		},
		{
			"nonce GET",
			signNonce(docSecret, docTime, "/v1/user/assets"),
			"f957817b95c3af6cf5e2e9dfe1503ea8088f46879d4ab73051467fd7b94f1aba",
		},
		{
			"nonce POST",
			signNonce(docSecret, docTime, docBody),
			"8ef83c2b991765b18c95aade7678471747c06890a23a453c76238345b5c86fb8",
		},
	} {
		if tc.got != tc.want {
			t.Errorf("%s:\n got  %s\n want %s", tc.name, tc.got, tc.want)
		}
	}
}

func TestTheSecretNeverSurvivesAnError(t *testing.T) {
	t.Parallel()
	c := &Client{key: "key-abcdefghijklmnop", secret: "secret-abcdefghijklmnop"}

	// The shape that actually happens: a server echoes the request back in an
	// error body, and that error is written into a JSONL record forever.
	got := c.redact("bitbank 401: bad auth for key-abcdefghijklmnop using secret-abcdefghijklmnop")
	for _, leak := range []string{"key-abcdefghijklmnop", "secret-abcdefghijklmnop"} {
		if contains(got, leak) {
			t.Errorf("credential survived redaction: %s", got)
		}
	}
	// And the diagnostic has to survive, or the redaction has made the log
	// useless instead of safe.
	if !contains(got, "401") {
		t.Errorf("redaction ate the diagnostic: %s", got)
	}
}

func TestAShortCredentialIsNotUsedAsAPattern(t *testing.T) {
	t.Parallel()
	// A two-character secret would match half of any error text and shred it.
	// Real bitbank credentials are long; a short one here means a config
	// mistake, and mangling the error would hide the mistake.
	c := &Client{key: "ab", secret: "cd"}
	const msg = "bitbank 10002: malformed request"
	if got := c.redact(msg); got != msg {
		t.Errorf("a two-character secret rewrote the message: %s", got)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
