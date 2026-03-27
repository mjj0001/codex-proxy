package auth

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
)

/**
 * IngestResult 账号导入结果
 */
type IngestResult struct {
	Added      int      `json:"added"`
	Updated    int      `json:"updated"`
	Failed     int      `json:"failed"`
	PoolTotal  int      `json:"pool_total"`
	Errors     []string `json:"errors,omitempty"`
	maxErrKeep int
}

const ingestMaxErrors = 48

func (r *IngestResult) appendErr(msg string) {
	if r.maxErrKeep == 0 {
		r.maxErrKeep = ingestMaxErrors
	}
	if len(r.Errors) >= r.maxErrKeep {
		return
	}
	r.Errors = append(r.Errors, msg)
}

/**
 * parseTokenFilePayloads 解析请求体：JSON 数组、单个 JSON 对象，或 NDJSON（每行一个对象）
 */
func parseTokenFilePayloads(body []byte) ([]TokenFile, error) {
	body = bytes.TrimSpace(body)
	if len(body) == 0 {
		return nil, fmt.Errorf("空请求体")
	}
	switch body[0] {
	case '[':
		var arr []TokenFile
		if err := json.Unmarshal(body, &arr); err != nil {
			return nil, fmt.Errorf("解析 JSON 数组失败: %w", err)
		}
		return arr, nil
	case '{':
		var one TokenFile
		if err := json.Unmarshal(body, &one); err != nil {
			return nil, fmt.Errorf("解析 JSON 对象失败: %w", err)
		}
		return []TokenFile{one}, nil
	default:
		return parseNDJSONTokenFiles(body)
	}
}

func parseNDJSONTokenFiles(body []byte) ([]TokenFile, error) {
	lines := bytes.Split(body, []byte("\n"))
	out := make([]TokenFile, 0, len(lines))
	for i, line := range lines {
		line = bytes.TrimSpace(line)
		if len(line) == 0 || line[0] == '#' {
			continue
		}
		var tf TokenFile
		if err := json.Unmarshal(line, &tf); err != nil {
			return nil, fmt.Errorf("第 %d 行 NDJSON 解析失败: %w", i+1, err)
		}
		out = append(out, tf)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("NDJSON 中无有效对象")
	}
	return out, nil
}

func sanitizeAuthFileBase(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			b.WriteRune(r)
		case r == '@', r == '+':
			b.WriteRune('_')
		default:
			b.WriteRune('_')
		}
	}
	out := strings.Trim(b.String(), "._-")
	if out == "" {
		return ""
	}
	if len(out) > 200 {
		out = out[:200]
	}
	return out
}

func ingestSyntheticAccountID(refreshToken string) string {
	h := sha256.Sum256([]byte(refreshToken))
	return "upload_" + hex.EncodeToString(h[:8])
}

/**
 * ensureIngestDBIdentity 数据库模式下保证 account_id 与 email 至少有一个非空，以便 upsert 与 FilePath 稳定
 */
func ensureIngestDBIdentity(acc *Account) {
	if strings.TrimSpace(acc.Token.AccountID) == "" && strings.TrimSpace(acc.Token.Email) == "" {
		acc.Token.AccountID = ingestSyntheticAccountID(acc.Token.RefreshToken)
	}
}

func (m *Manager) ingestFilePathForAccount(acc *Account) string {
	if m.db != nil {
		aid := strings.TrimSpace(acc.Token.AccountID)
		em := strings.TrimSpace(acc.Token.Email)
		if aid != "" {
			return "db:" + aid
		}
		if em != "" {
			return "db:" + em
		}
		return "db:" + ingestSyntheticAccountID(acc.Token.RefreshToken)
	}
	base := sanitizeAuthFileBase(acc.Token.Email)
	if base == "" {
		base = sanitizeAuthFileBase(acc.Token.AccountID)
	}
	if base == "" {
		base = ingestSyntheticAccountID(acc.Token.RefreshToken)
	}
	return filepath.Join(m.authDir, base+".json")
}

/**
 * IngestAccountsFromJSON 将 JSON 凭据写入号池：与磁盘 *.json / 数据库 upsert 一致，已存在同一路径（db:… 或文件路径）则更新 Token
 */
func (m *Manager) IngestAccountsFromJSON(body []byte) (IngestResult, error) {
	if m.db == nil && strings.TrimSpace(m.authDir) == "" {
		return IngestResult{}, fmt.Errorf("未配置 auth-dir 且未启用数据库，无法导入")
	}
	tokens, err := parseTokenFilePayloads(body)
	if err != nil {
		return IngestResult{}, err
	}
	if m.db != nil {
		m.importMu.Lock()
		defer m.importMu.Unlock()
	}

	var res IngestResult
	for i, tf := range tokens {
		acc, aerr := accountFromTokenFile(&tf, "")
		if aerr != nil {
			res.Failed++
			res.appendErr(fmt.Sprintf("#%d: %v", i+1, aerr))
			continue
		}
		if m.db != nil {
			ensureIngestDBIdentity(acc)
		}
		acc.FilePath = m.ingestFilePathForAccount(acc)

		m.mu.Lock()
		if ex, ok := m.accountIndex[acc.FilePath]; ok {
			ex.UpdateToken(acc.TokenSnapshot())
			m.mu.Unlock()
			m.enqueueSave(ex)
			res.Updated++
		} else {
			m.accounts = append(m.accounts, acc)
			m.accountIndex[acc.FilePath] = acc
			m.publishSnapshot()
			m.mu.Unlock()
			m.enqueueSave(acc)
			res.Added++
		}
	}
	if res.Added+res.Updated > 0 {
		m.InvalidateSelectorCache()
	}
	m.mu.RLock()
	res.PoolTotal = len(m.accounts)
	m.mu.RUnlock()
	return res, nil
}
