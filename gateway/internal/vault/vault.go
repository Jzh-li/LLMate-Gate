// Package vault 实现加密映射表（契约 §7）。
//
// 安全模型：映射表只驻留内存（请求生命周期），加密密钥由 scrypt 派生且不落盘。
// 这是刻意的取舍——密钥不落盘意味着旧映射表在重启后必然不可读，原文不会在磁盘上
// 留下一份可解密副本；代价是映射表无法跨重启存活。
//
// Seal/Unseal 仍按契约 §7.2 提供（序列化 + AES-256-GCM 往返，供测试与外部调用方
// 自行处理密文），但 vault 自身不把密文写到磁盘。历史上有一个 vault.persist 开关
// 这么做，而它用进程内随机密钥加密、Get 又只查内存 map：结果是磁盘上不断累积
// 永远解不开也永远读不到的文件，用户却以为「已经持久化了」。该开关已在配置层
// 明确拒绝（见 config.Validate）。
package vault

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"sync"
	"time"

	gatewayerrors "gateway/internal/errors"
	"gateway/pkg/types"

	"golang.org/x/crypto/scrypt"
)

// magic 落盘文件头标识。
var magic = []byte("LMGV")

// Vault 映射表存储抽象（契约 §7.2）。
type Vault interface {
	Put(table *types.MappingTable) error
	Get(requestID string) (*types.MappingTable, error)
	Seal(table *types.MappingTable) ([]byte, error)
	Unseal(data []byte) (*types.MappingTable, error)
	Sweep() (int, error)
	// Len 当前存活映射表数量（供 /metrics）。
	Len() int
}

// ErrExpired 映射表过期（对外仍统一为 not_found，避免信息泄露）。
var ErrExpired = errors.New("mapping table expired")

// MemVault 进程内映射表存储 + TTL 自动清理（契约 §7.2/§8.2）。
type MemVault struct {
	mu     sync.RWMutex
	tables map[string]*types.MappingTable
	ttl    time.Duration
	key    []byte
	gcm    cipher.AEAD
}

// NewMemVault 构造内存 vault。passphrase 为空时随机生成（仅内存存活，重启即失效）。
func NewMemVault(ttl time.Duration, passphrase []byte) (*MemVault, error) {
	if ttl <= 0 {
		ttl = 30 * time.Minute
	}
	if len(passphrase) == 0 {
		passphrase = make([]byte, 32)
		if _, err := io.ReadFull(rand.Reader, passphrase); err != nil {
			return nil, gatewayerrors.Wrap(gatewayerrors.CodeVaultSealFailed, "generate vault key", err)
		}
	}
	// scrypt(salt=固定域分隔 salt, N=32768)（契约 §7.3）
	key, err := scrypt.Key(passphrase, []byte("llmate-gate/vault/v1"), 32768, 8, 1, 32)
	if err != nil {
		return nil, gatewayerrors.Wrap(gatewayerrors.CodeVaultSealFailed, "derive vault key", err)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, gatewayerrors.Wrap(gatewayerrors.CodeVaultSealFailed, "init aes", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, gatewayerrors.Wrap(gatewayerrors.CodeVaultSealFailed, "init gcm", err)
	}
	return &MemVault{
		tables: make(map[string]*types.MappingTable),
		ttl:    ttl,
		key:    key,
		gcm:    gcm,
	}, nil
}

// zeroKey 销毁派生密钥。
func (v *MemVault) zeroKey() {
	for i := range v.key {
		v.key[i] = 0
	}
}

// Put 存储映射表并设置 TTL（契约 §7.2）。
//
// 同一 request_id 上已有未过期条目时拒绝写入，而不是静默覆盖。request_id 是映射表
// 主键，覆盖会同时造成两件事：先那次请求的占位符永远还原不回来（对用户表现为
// 「还原结果串了」的静默损坏），以及把两次请求的原文混进同一个可还原键（一条
// 「用已知 ID 覆盖并读取他人映射」的路径）。
//
// 正常路径上 request_id 是 32 字符随机 hex（见 proxy.requestID），碰撞概率可忽略，
// 拒绝不影响正常流量；受影响的是「客户端固定传同一个 X-Request-ID」这种用法，
// 而那种用法本来就是错的——它必然覆盖，只是从前失败得静默。
func (v *MemVault) Put(table *types.MappingTable) error {
	if table == nil || table.RequestID == "" {
		return gatewayerrors.New(gatewayerrors.CodeInvalidRequest, "mapping table requires request_id")
	}
	if table.CreatedAt.IsZero() {
		table.CreatedAt = time.Now()
	}
	if table.ExpiresAt.IsZero() {
		table.ExpiresAt = table.CreatedAt.Add(v.ttl)
	}
	v.mu.Lock()
	if prev, ok := v.tables[table.RequestID]; ok && !prev.Expired(time.Now()) {
		v.mu.Unlock()
		return gatewayerrors.Errorf(gatewayerrors.CodeVaultSealFailed,
			"request_id %q already holds an unexpired mapping table (refusing to overwrite)", table.RequestID)
	}
	v.tables[table.RequestID] = table
	v.mu.Unlock()
	return nil
}

// Get 按 RequestID 加载；过期或不存在返回 not_found（契约 §7.3）。
func (v *MemVault) Get(requestID string) (*types.MappingTable, error) {
	v.mu.RLock()
	t, ok := v.tables[requestID]
	v.mu.RUnlock()
	if !ok {
		return nil, gatewayerrors.ErrNotFound
	}
	if t.Expired(time.Now()) {
		_ = v.Delete(requestID)
		return nil, gatewayerrors.ErrNotFound
	}
	return t, nil
}

// Delete 删除并清零映射表明文（契约 §7.4）。
func (v *MemVault) Delete(requestID string) error {
	v.mu.Lock()
	t, ok := v.tables[requestID]
	if ok {
		delete(v.tables, requestID)
	}
	v.mu.Unlock()
	if ok {
		t.Zeroize()
	}
	return nil
}

// Seal 序列化 + AES-256-GCM 加密（契约 §7.2/§7.3）。
func (v *MemVault) Seal(table *types.MappingTable) ([]byte, error) {
	plain, err := json.Marshal(table)
	if err != nil {
		return nil, gatewayerrors.Wrap(gatewayerrors.CodeVaultSealFailed, "marshal mapping table", err)
	}
	nonce := make([]byte, v.gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, gatewayerrors.Wrap(gatewayerrors.CodeVaultSealFailed, "generate nonce", err)
	}
	ct := v.gcm.Seal(nil, nonce, plain, nil)
	out := make([]byte, 0, len(magic)+len(nonce)+len(ct))
	out = append(out, magic...)
	out = append(out, nonce...)
	out = append(out, ct...)
	return out, nil
}

// Unseal 解密 + 反序列化（契约 §7.2）。
func (v *MemVault) Unseal(data []byte) (*types.MappingTable, error) {
	if len(data) < len(magic)+v.gcm.NonceSize() {
		return nil, gatewayerrors.New(gatewayerrors.CodeVaultSealFailed, "sealed blob too short")
	}
	if string(data[:len(magic)]) != string(magic) {
		return nil, gatewayerrors.New(gatewayerrors.CodeVaultSealFailed, "bad vault magic")
	}
	nonce := data[len(magic) : len(magic)+v.gcm.NonceSize()]
	ct := data[len(magic)+len(nonce):]
	plain, err := v.gcm.Open(nil, nonce, ct, nil)
	if err != nil {
		return nil, gatewayerrors.Wrap(gatewayerrors.CodeVaultSealFailed, "decrypt mapping table", err)
	}
	var t types.MappingTable
	if err := json.Unmarshal(plain, &t); err != nil {
		return nil, gatewayerrors.Wrap(gatewayerrors.CodeVaultSealFailed, "decode mapping table", err)
	}
	return &t, nil
}

// Sweep 清理过期条目，返回清理数量（契约 §7.2）。
func (v *MemVault) Sweep() (int, error) {
	now := time.Now()
	var expired []string
	v.mu.RLock()
	for id, t := range v.tables {
		if t.Expired(now) {
			expired = append(expired, id)
		}
	}
	v.mu.RUnlock()
	for _, id := range expired {
		_ = v.Delete(id)
	}
	return len(expired), nil
}

// Len 当前存活映射表数量（供 /healthz）。
func (v *MemVault) Len() int {
	v.mu.RLock()
	defer v.mu.RUnlock()
	return len(v.tables)
}

// StartSweeper 后台定期清理（契约 §7.2）。
func (v *MemVault) StartSweeper(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = time.Minute
	}
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				_, _ = v.Sweep()
			}
		}
	}()
}

// Close 销毁密钥与全部明文。
func (v *MemVault) Close() error {
	v.mu.Lock()
	for id, t := range v.tables {
		t.Zeroize()
		delete(v.tables, id)
	}
	v.mu.Unlock()
	v.zeroKey()
	return nil
}

// 编译期接口断言。
var _ Vault = (*MemVault)(nil)
