package auth

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

const (
	iss = "https://manager.test"
	aud = "https://chat.test"
)

var t0 = time.Unix(1_800_000_000, 0)

func setup(t *testing.T) (ed25519.PrivateKey, *Verifier) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	v := NewVerifier([]ed25519.PublicKey{pub}, iss, aud)
	v.Now = func() time.Time { return t0 }
	return priv, v
}

func claims() *Claims {
	return &Claims{Issuer: iss, Audience: aud, Subject: 42, Email: "p@example.com", Name: "Priya", Role: RoleCustomer,
		Accounts: []int64{7}, Sites: []int64{11, 12}, IssuedAt: t0.Unix(), Expires: t0.Add(10 * time.Minute).Unix(), ID: "abc"}
}

func TestRoundTrip(t *testing.T) {
	priv, v := setup(t)
	tok, err := Sign(priv, claims())
	if err != nil {
		t.Fatal(err)
	}
	c, err := v.Verify(tok)
	if err != nil {
		t.Fatal(err)
	}
	if c.Subject != 42 || !c.HasSite(12) || c.HasSite(13) || !c.HasAccount(7) || c.IsOperator() {
		t.Fatalf("claims wrong: %+v", c)
	}
}

func TestRejects(t *testing.T) {
	priv, v := setup(t)
	_, otherPriv, _ := ed25519.GenerateKey(rand.Reader)
	mut := func(f func(*Claims)) string {
		c := claims()
		f(c)
		tok, _ := Sign(priv, c)
		return tok
	}
	good, _ := Sign(priv, claims())
	parts := strings.Split(good, ".")
	forged, _ := Sign(otherPriv, claims())
	escalated := mut(func(c *Claims) { c.Role = RoleOperator }) // validly signed operator token, used below as a payload donor

	cases := map[string]string{
		"empty":           "",
		"wrong key":       forged,
		"wrong prefix":    "v2." + parts[1] + "." + parts[2],
		"swapped payload": "v1." + strings.Split(escalated, ".")[1] + "." + parts[2],
		"truncated sig":   parts[0] + "." + parts[1] + "." + parts[2][:20],
		"wrong issuer":    mut(func(c *Claims) { c.Issuer = "https://evil.test" }),
		"wrong audience":  mut(func(c *Claims) { c.Audience = "https://other-chat.test" }),
		"bad role":        mut(func(c *Claims) { c.Role = "admin" }),
		"no subject":      mut(func(c *Claims) { c.Subject = 0 }),
		"expired": mut(func(c *Claims) {
			c.IssuedAt = t0.Add(-20 * time.Minute).Unix()
			c.Expires = t0.Add(-2 * time.Minute).Unix()
		}),
		"too long lived": mut(func(c *Claims) { c.Expires = t0.Add(2 * time.Hour).Unix() }),
		"future issued": mut(func(c *Claims) {
			c.IssuedAt = t0.Add(10 * time.Minute).Unix()
			c.Expires = t0.Add(15 * time.Minute).Unix()
		}),
		"oversize":       strings.Repeat("a", MaxTokenBytes+1),
		"four parts":     good + ".x",
		"exp before iat": mut(func(c *Claims) { c.Expires = c.IssuedAt - 1 }),
		"no issued at":   mut(func(c *Claims) { c.IssuedAt = 0 }),
	}
	for name, tok := range cases {
		if _, err := v.Verify(tok); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: want ErrInvalid, got %v", name, err)
		}
	}
}

func TestSkewTolerated(t *testing.T) {
	priv, v := setup(t)
	c := claims()
	c.IssuedAt = t0.Add(30 * time.Second).Unix() // Manager clock 30s ahead
	c.Expires = t0.Add(10 * time.Minute).Unix()
	tok, _ := Sign(priv, c)
	if _, err := v.Verify(tok); err != nil {
		t.Fatalf("30s skew should pass: %v", err)
	}
}

func TestKeyRotation(t *testing.T) {
	oldPub, oldPriv, _ := ed25519.GenerateKey(rand.Reader)
	newPub, _, _ := ed25519.GenerateKey(rand.Reader)
	v := NewVerifier([]ed25519.PublicKey{newPub, oldPub}, iss, aud)
	v.Now = func() time.Time { return t0 }
	tok, _ := Sign(oldPriv, claims())
	if _, err := v.Verify(tok); err != nil {
		t.Fatalf("old key still listed should verify: %v", err)
	}
}

// TestPHPSodiumSigner signs with PHP's sodium extension exactly as the
// Manager will, then verifies in Go. Skips when php or sodium is missing.
func TestPHPSodiumSigner(t *testing.T) {
	php, err := exec.LookPath("php")
	if err != nil {
		t.Skip("php not installed")
	}
	now := time.Now()
	script := `
if (!function_exists('sodium_crypto_sign_keypair')) { fwrite(STDERR, "no sodium"); exit(3); }
$kp  = sodium_crypto_sign_keypair();
$sk  = sodium_crypto_sign_secretkey($kp);
$pk  = sodium_crypto_sign_publickey($kp);
$b64 = function ($s) { return rtrim(strtr(base64_encode($s), '+/', '-_'), '='); };
$claims = array(
	'iss' => '` + iss + `', 'aud' => '` + aud + `', 'sub' => 42, 'email' => 'p@example.com',
	'name' => 'Priya "P" <Shah> & Co', 'role' => 'customer', 'accounts' => array(7), 'sites' => array(11),
	'iat' => ` + strconv.FormatInt(now.Unix(), 10) + `, 'exp' => ` + strconv.FormatInt(now.Add(10*time.Minute).Unix(), 10) + `,
	'jti' => bin2hex(random_bytes(8)),
);
$payload = $b64(json_encode($claims, JSON_UNESCAPED_SLASHES | JSON_UNESCAPED_UNICODE));
$sig     = sodium_crypto_sign_detached('captaincore-chat.v1.' . $payload, $sk);
echo base64_encode($pk), "\n", 'v1.' . $payload . '.' . $b64($sig), "\n";
`
	out, err := exec.Command(php, "-r", script).CombinedOutput()
	if err != nil {
		if strings.Contains(string(out), "no sodium") {
			t.Skip("php sodium extension missing")
		}
		t.Fatalf("php: %v\n%s", err, out)
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) != 2 {
		t.Fatalf("unexpected php output: %q", out)
	}
	pub, err := base64.StdEncoding.DecodeString(lines[0])
	if err != nil {
		t.Fatal(err)
	}
	v := NewVerifier([]ed25519.PublicKey{pub}, iss, aud)
	c, err := v.Verify(lines[1])
	if err != nil {
		t.Fatalf("PHP-signed token rejected: %v", err)
	}
	if c.Name != `Priya "P" <Shah> & Co` || c.Subject != 42 || !c.HasSite(11) {
		t.Fatalf("claims wrong: %+v", c)
	}
}

// TestPHPPublishSigner signs a publish body the way the Manager does and
// checks it here, including a non-ASCII body sent unescaped.
func TestPHPPublishSigner(t *testing.T) {
	php, err := exec.LookPath("php")
	if err != nil {
		t.Skip("php not installed")
	}
	script := `
if (!function_exists('sodium_crypto_sign_keypair')) { fwrite(STDERR, "no sodium"); exit(3); }
$kp   = sodium_crypto_sign_keypair();
$b64  = function ($s) { return rtrim(strtr(base64_encode($s), '+/', '-_'), '='); };
$body = json_encode(array('event' => array('type' => 'message.created', 'message' => array('body' => "Merci beaucoup 👍 </script>")), 'customer_user_id' => 42, 'internal' => false), JSON_UNESCAPED_SLASHES | JSON_UNESCAPED_UNICODE);
$ts   = (string) time();
$sig  = sodium_crypto_sign_detached('captaincore-chat.publish.v1.' . $ts . '.' . $body, sodium_crypto_sign_secretkey($kp));
echo base64_encode(sodium_crypto_sign_publickey($kp)), "\n", $ts, "\n", $b64($sig), "\n", base64_encode($body), "\n";
`
	out, err := exec.Command(php, "-r", script).CombinedOutput()
	if err != nil {
		if strings.Contains(string(out), "no sodium") {
			t.Skip("php sodium extension missing")
		}
		t.Fatalf("php: %v\n%s", err, out)
	}
	l := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(l) != 4 {
		t.Fatalf("unexpected php output: %q", out)
	}
	pub, _ := base64.StdEncoding.DecodeString(l[0])
	body, _ := base64.StdEncoding.DecodeString(l[3])
	v := NewVerifier([]ed25519.PublicKey{pub}, iss, aud)
	if err := v.VerifyPublish(l[1], body, l[2]); err != nil {
		t.Fatalf("PHP-signed publish rejected: %v", err)
	}
	if err := v.VerifyPublish(l[1], append(body, ' '), l[2]); err == nil {
		t.Fatal("a changed body must not verify")
	}
}
