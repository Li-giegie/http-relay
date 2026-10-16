package auth

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestCreateAndVerify 验证签发的 token 能通过校验，且格式符合预期。
func TestCreateAndVerify(t *testing.T) {
	s := NewTokenStore()
	token, info, err := s.Create("", "test-token", time.Hour, 0)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// relay_ + 32 字节 base64url = 6 + 43 字符
	if len(token) != len(tokenPrefix)+43 || !strings.HasPrefix(token, tokenPrefix) {
		t.Fatalf("token = %q, want %q prefix + 43 base64url chars", token, tokenPrefix)
	}
	if !s.Verify(token) {
		t.Fatal("Verify(recently created token) = false, want true")
	}
	if info.Name != "test-token" {
		t.Errorf("info.Name = %q, want %q", info.Name, "test-token")
	}
	if len(info.ID) == 0 {
		t.Error("info.ID is empty")
	}
}

// TestVerifyRejectsWrong 验证未知/篡改 token 被拒绝。
func TestVerifyRejectsWrong(t *testing.T) {
	s := NewTokenStore()
	token, _, err := s.Create("", "t", time.Hour, 0)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// 另一个 store 没有该记录。
	other := NewTokenStore()
	for i, bad := range []string{
		"",
		"garbage",
		token + "x",
		token[:len(token)-1],
		"relay_" + token,
	} {
		if s.Verify(bad) {
			t.Errorf("case %d: Verify(%q) = true, want false", i, bad)
		}
	}
	if other.Verify(token) {
		t.Error("token from another store verified, want false")
	}
}

// TestVerifyExpired 验证过期 token 被拒绝。
func TestVerifyExpired(t *testing.T) {
	s := NewTokenStore()
	token, _, err := s.Create("", "expired", -time.Hour, 0) // 已过期
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if s.Verify(token) {
		t.Fatal("Verify(expired token) = true, want false")
	}
	// 过期记录在 List 时被清理
	if got := len(s.List()); got != 0 {
		t.Errorf("List length = %d, want 0 (expired record pruned)", got)
	}
}

// TestMaxUses 验证使用次数上限：第 maxUses 次成功，之后拒绝。
func TestMaxUses(t *testing.T) {
	s := NewTokenStore()
	token, _, err := s.Create("", "limited", time.Hour, 3)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	for i := 1; i <= 3; i++ {
		if !s.Verify(token) {
			t.Fatalf("Verify #%d = false, want true", i)
		}
	}
	if s.Verify(token) {
		t.Fatal("Verify #4 = true, want false (max uses reached)")
	}
	if s.Verify(token) { // 超限后继续拒绝
		t.Fatal("Verify #5 = true, want false")
	}
}

// TestVerifyUpdatesUsage 验证命中后 Uses 与 LastUsed 被更新。
func TestVerifyUpdatesUsage(t *testing.T) {
	s := NewTokenStore()
	token, _, err := s.Create("", "t", time.Hour, 0)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	list := s.List()
	if list[0].Uses != 0 || !list[0].LastUsed.IsZero() {
		t.Fatalf("initial Uses/LastUsed = %d/%v, want 0/zero", list[0].Uses, list[0].LastUsed)
	}
	s.Verify(token)
	list = s.List()
	if list[0].Uses != 1 || list[0].LastUsed.IsZero() {
		t.Fatalf("after Verify: Uses/LastUsed = %d/%v, want 1/non-zero", list[0].Uses, list[0].LastUsed)
	}
}

// TestDelete 验证删除（撤销）后 token 立即失效。
func TestDelete(t *testing.T) {
	s := NewTokenStore()
	token, info, err := s.Create("", "doomed", time.Hour, 0)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if !s.Delete(info.ID) {
		t.Fatal("Delete(existing id) = false")
	}
	if s.Verify(token) {
		t.Fatal("Verify after Delete = true")
	}
	if s.Delete(info.ID) {
		t.Fatal("Delete(same id twice) = true")
	}
	if got := len(s.List()); got != 0 {
		t.Fatalf("List length = %d, want 0", got)
	}
}

// TestUnlimitedToken 验证 ttl=0 且 maxUses=0 时永不过期、不限次数。
func TestUnlimitedToken(t *testing.T) {
	s := NewTokenStore()
	token, info, err := s.Create("", "forever", 0, 0)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if !info.ExpiresAt.IsZero() || info.MaxUses != 0 {
		t.Fatalf("info ExpiresAt/MaxUses = %v/%d, want zero/0", info.ExpiresAt, info.MaxUses)
	}
	for i := 0; i < 10; i++ {
		if !s.Verify(token) {
			t.Fatalf("Verify #%d = false, want true (unlimited)", i+1)
		}
	}
}

// TestCreateCustomToken 验证手动录入的 token：原样保存可校验，
// 重复录入与非可见 ASCII 字符被拒绝。
func TestCreateCustomToken(t *testing.T) {
	s := NewTokenStore()
	const custom = "my-own-token-123"
	plaintext, info, err := s.Create(custom, "manual", time.Hour, 0)
	if err != nil {
		t.Fatalf("create custom: %v", err)
	}
	if plaintext != custom || info.Token != custom {
		t.Fatalf("plaintext/info.Token = %q/%q, want %q (custom token kept as-is)", plaintext, info.Token, custom)
	}
	if !s.Verify(custom) {
		t.Fatal("Verify(custom token) = false, want true")
	}

	// 重复录入 → ErrTokenExists
	if _, _, err := s.Create(custom, "dup", time.Hour, 0); !errors.Is(err, ErrTokenExists) {
		t.Fatalf("duplicate create err = %v, want ErrTokenExists", err)
	}
	// 与已自动生成的 token 重复 → 同样拒绝
	auto, _, err := s.Create("", "auto", time.Hour, 0)
	if err != nil {
		t.Fatalf("create auto: %v", err)
	}
	if _, _, err := s.Create(auto, "dup-auto", time.Hour, 0); !errors.Is(err, ErrTokenExists) {
		t.Fatalf("duplicate auto err = %v, want ErrTokenExists", err)
	}

	// 非法格式 → ErrInvalidToken
	for i, bad := range []string{
		"has space",
		"has\ttab",
		"中文token",
		"ctrl\x01char",
		"trailing\n",
		strings.Repeat("x", 129), // 超长
	} {
		if _, _, err := s.Create(bad, "bad", time.Hour, 0); !errors.Is(err, ErrInvalidToken) {
			t.Errorf("case %d %q: err = %v, want ErrInvalidToken", i, bad, err)
		}
	}
	// 长度上限边界：正好 128 字符合法
	if _, _, err := s.Create(strings.Repeat("x", 128), "edge", time.Hour, 0); err != nil {
		t.Errorf("128-char token rejected: %v", err)
	}
}

// TestTokenUniqueness 验证连续签发不产生重复 token。
func TestTokenUniqueness(t *testing.T) {
	s := NewTokenStore()
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		token, _, err := s.Create("", "u", time.Hour, 0)
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		if seen[token] {
			t.Fatalf("duplicate token generated: %q", token)
		}
		seen[token] = true
	}
}

// TestConcurrentVerify 并发签发/校验/删除（-race）。
func TestConcurrentVerify(t *testing.T) {
	s := NewTokenStore()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			token, info, err := s.Create("", "c", time.Hour, 0)
			if err != nil {
				t.Errorf("create: %v", err)
				return
			}
			for j := 0; j < 10; j++ {
				s.Verify(token)
				s.List()
			}
			s.Delete(info.ID)
		}()
	}
	wg.Wait()
}
