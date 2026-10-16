// Package auth 提供不透明 token（opaque token）的签发与校验：
// token 为高熵随机串或管理员手动指定的值，本身不携带任何信息；
// 过期时间、使用次数上限、撤销等全部由内存中的签发记录决定，验证即查表。
// 查表按 token 的 SHA-256 进行，避免以明文作 key 的时序泄露。
// 纯内存存储：签发记录随进程存活，重启后全部失效（未来可换 sqlite 等持久化）。
package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"
)

// tokenPrefix 自动生成 token 的固定前缀，便于辨认归属（仅为界面提示，不参与校验）。
const tokenPrefix = "relay_"

// maxCustomTokenLen 手动录入 token 的长度上限（需能放进请求头）。
const maxCustomTokenLen = 128

// ErrInvalidToken 表示手动录入的 token 格式非法（含错误细节）。
var ErrInvalidToken = errors.New("invalid token")

// ErrTokenExists 表示手动录入的 token 与已签发的 token 重复。
var ErrTokenExists = errors.New("token already exists")

// TokenInfo 是一条签发记录（内存中保存，界面可见）。
type TokenInfo struct {
	ID        string    `json:"id"`         // 记录唯一 id（删除/界面用）
	Name      string    `json:"name"`       // 备注名
	Token     string    `json:"token"`      // 完整明文，可见可复制
	CreatedAt time.Time `json:"created_at"` // 签发时间
	ExpiresAt time.Time `json:"expires_at"` // 过期时间（零值 = 永不过期）
	MaxUses   int64     `json:"max_uses"`   // 使用次数上限（0 = 不限）
	Uses      int64     `json:"uses"`       // 已使用次数
	LastUsed  time.Time `json:"last_used"`  // 最后使用时间
}

// TokenStore 在内存中管理已签发的 token。
type TokenStore struct {
	mu sync.RWMutex
	m  map[string]*TokenInfo // key = SHA-256(token)
}

// NewTokenStore 创建空 store。
func NewTokenStore() *TokenStore {
	return &TokenStore{m: map[string]*TokenInfo{}}
}

// Create 签发一个新 token 并保存记录。
// token 有两种来源：customToken 非空时为手动录入（管理员自定值），
// 为空时自动生成高熵随机串。手动录入的 token 需为可见 ASCII 字符
// （0x21-0x7E，不含空格与控制字符）、不超过 maxCustomTokenLen 字符，
// 且不得与已签发的 token 重复（否则返回错误）。
// ttl 为正表示有效期；为负表示签发即已过期（测试用）；为 0 表示永不过期。
// maxUses <= 0 表示不限次数。
func (s *TokenStore) Create(customToken, name string, ttl time.Duration, maxUses int64) (plaintext string, info TokenInfo, err error) {
	if customToken != "" {
		if err := validateCustomToken(customToken); err != nil {
			return "", TokenInfo{}, err
		}
		plaintext = customToken
	} else {
		raw := make([]byte, 32)
		if _, err := rand.Read(raw); err != nil {
			return "", TokenInfo{}, fmt.Errorf("generate token: %w", err)
		}
		plaintext = tokenPrefix + base64.RawURLEncoding.EncodeToString(raw)
	}

	id := make([]byte, 16)
	if _, err := rand.Read(id); err != nil {
		return "", TokenInfo{}, fmt.Errorf("generate token id: %w", err)
	}
	now := time.Now()
	info = TokenInfo{
		ID:        hex.EncodeToString(id),
		Name:      name,
		Token:     plaintext,
		CreatedAt: now,
		MaxUses:   maxUses,
	}
	if ttl != 0 {
		info.ExpiresAt = now.Add(ttl)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	h := hashToken(plaintext)
	if _, dup := s.m[h]; dup {
		return "", TokenInfo{}, fmt.Errorf("%w: %q", ErrTokenExists, customToken)
	}
	cp := info
	s.m[h] = &cp
	return plaintext, info, nil
}

// validateCustomToken 校验手动录入的 token：
// 仅允许可见 ASCII 字符（token 要放进请求头/查询参数，不能含空格与控制字符），
// 且长度不超过 maxCustomTokenLen。
func validateCustomToken(token string) error {
	if len(token) > maxCustomTokenLen {
		return fmt.Errorf("%w: %q exceeds %d characters", ErrInvalidToken, token, maxCustomTokenLen)
	}
	for i := 0; i < len(token); i++ {
		if c := token[i]; c < 0x21 || c > 0x7E {
			return fmt.Errorf("%w: %q contains non-printable or whitespace character %q", ErrInvalidToken, token, rune(c))
		}
	}
	return nil
}

// Verify 校验 token：查表 → 未过期 → 次数未用尽。
// 通过后计数 +1 并更新 LastUsed；记录被删除（撤销）即查无此 token。
func (s *TokenStore) Verify(plaintext string) bool {
	if plaintext == "" {
		return false
	}
	h := hashToken(plaintext)

	s.mu.Lock()
	defer s.mu.Unlock()
	info, ok := s.m[h]
	if !ok {
		return false
	}
	if !info.ExpiresAt.IsZero() && time.Now().After(info.ExpiresAt) {
		return false
	}
	if info.MaxUses > 0 && info.Uses >= info.MaxUses {
		return false
	}
	info.Uses++
	info.LastUsed = time.Now()
	return true
}

// Delete 按记录 id 删除（= 撤销该 token），返回记录是否存在。
func (s *TokenStore) Delete(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for h, info := range s.m {
		if info.ID == id {
			delete(s.m, h)
			return true
		}
	}
	return false
}

// List 返回全部签发记录（按创建时间升序），顺带清理已过期的记录。
func (s *TokenStore) List() []TokenInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	for h, info := range s.m {
		if !info.ExpiresAt.IsZero() && now.After(info.ExpiresAt) {
			delete(s.m, h)
		}
	}
	list := make([]TokenInfo, 0, len(s.m))
	for _, info := range s.m {
		list = append(list, *info)
	}
	sort.SliceStable(list, func(i, j int) bool {
		return list[i].CreatedAt.Before(list[j].CreatedAt)
	})
	return list
}

// hashToken 返回明文 token 的 SHA-256 hex。
func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
