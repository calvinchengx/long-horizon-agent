package obs

import "testing"

// The cases of spec/obs/redact.json, repeated here so the package's own tests pin them (the mutation
// audit runs only these; internal/spec runs the JSON itself).

func TestSpecRedact(t *testing.T) {
	for _, c := range []struct{ text, want string }{
		{"key sk-ant-abcdefghijklmnop1234 here", "key *** here"},
		{"token ghp_abcdefghijklmnopqrstuvwxyz0123", "token ***"},
		{"github_pat_11ABCDEFGHIJKLMNOPQRST_abcdefghij", "***"},
		{"slack xoxb-1234567890-abcdefghij", "slack ***"},
		{"aws AKIAABCDEFGHIJKLMNOP done", "aws *** done"},
		{"google AIzaSyA1234567890abcdefghijklmnopqrstu", "google ***"},
		{"Authorization: Bearer abc.def.ghi", "Authorization: Bearer ***"},
		{"authorization=Basic dXNlcjpwYXNz", "authorization=Basic ***"},
		{"curl -H 'Authorization: Token t0ps3cret'", "curl -H 'Authorization: Token ***'"},
		{"bearer xyz123==", "bearer ***"},
		{"postgresql://lha:secretpw@db:5432/lha", "postgresql://lha:***@db:5432/lha"},
		{"redis://:hunter2secret@localhost:6379/0", "redis://:***@localhost:6379/0"},
		{"postgresql://u:p@ss@host/db", "postgresql://u:***@host/db"},
		{"https://example.com/path?q=1", "https://example.com/path?q=1"},
		{"plain text with no secrets", "plain text with no secrets"},
		{"x_sk-abcdefghijklmnopqrst", "x_***"},
		{"voyage key pa-AbCdEfGhIjKlMnOpQrStUvWxYz0123456789_-xy done", "voyage key *** done"},
		{"short pa-abcdefghijklmnopqrstuvwxyz01234 and spa-abcdefghijklmnopqrstuvwxyz0123456789", "short pa-abcdefghijklmnopqrstuvwxyz01234 and spa-abcdefghijklmnopqrstuvwxyz0123456789"},
	} {
		if got := RedactText(c.text); got != c.want {
			t.Errorf("RedactText(%q) = %q, want %q", c.text, got, c.want)
		}
	}
	for _, c := range []struct {
		key    string
		secret bool
	}{
		{"api_key", true},
		{"ANTHROPIC_API_KEY", true},
		{"password", true},
		{"db_password", true},
		{"secret", true},
		{"client_secret", true},
		{"token", true},
		{"access_token", true},
		{"accessToken", true},
		{"refreshToken", true},
		{"sessionToken", true},
		{"auth", true},
		{"authorization", true},
		{"dsn", true},
		{"postgres_dsn", true},
		{"tokens_used", false},
		{"input_tokens", false},
		{"author", false},
		{"name", false},
		{"private_key", true},
	} {
		if got := IsSecretKey(c.key); got != c.secret {
			t.Errorf("IsSecretKey(%q) = %v, want %v", c.key, got, c.secret)
		}
	}
}
