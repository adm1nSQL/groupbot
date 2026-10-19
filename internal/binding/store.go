package binding

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

var ErrEmailBound = errors.New("邮箱已被其他 Telegram 账号绑定")

// UserBinding 单个 Telegram 账号到面板账户的本地绑定
type UserBinding struct {
	Email  string `json:"email"`
	DBName string `json:"db_name"`
}

// Store 基于 JSON 文件的线程安全绑定存储
type Store struct {
	mu       sync.RWMutex
	filePath string
	// data key: telegram_id
	data map[int64]UserBinding
}

// New 打开（或初始化）绑定存储
func New(filePath string) (*Store, error) {
	s := &Store{
		filePath: filePath,
		data:     make(map[int64]UserBinding),
	}
	if err := s.load(); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("加载绑定文件失败: %w", err)
	}
	return s, nil
}

func (s *Store) load() error {
	raw, err := os.ReadFile(s.filePath)
	if err != nil {
		return err
	}
	if len(raw) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, &s.data); err != nil {
		return err
	}
	if s.data == nil {
		return fmt.Errorf("绑定文件内容不能为 null")
	}
	return nil
}

func (s *Store) save(data map[int64]UserBinding) error {
	raw, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(s.filePath)
	tmp, err := os.CreateTemp(dir, ".bindings-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), s.filePath)
}

func clone(data map[int64]UserBinding) map[int64]UserBinding {
	result := make(map[int64]UserBinding, len(data))
	for id, b := range data {
		result[id] = b
	}
	return result
}

func (s *Store) Set(telegramID int64, email, dbName string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, b := range s.data {
		if id != telegramID && b.Email == email && b.DBName == dbName {
			return ErrEmailBound
		}
	}
	next := clone(s.data)
	next[telegramID] = UserBinding{Email: email, DBName: dbName}
	if err := s.save(next); err != nil {
		return err
	}
	s.data = next
	return nil
}

// Transfer 验证完成后原子地迁移绑定，返回原绑定的 Telegram ID。
func (s *Store) Transfer(telegramID int64, email, dbName string) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := clone(s.data)
	var oldID int64
	for id, b := range next {
		if id != telegramID && b.Email == email && b.DBName == dbName {
			oldID = id
			delete(next, id)
		}
	}
	next[telegramID] = UserBinding{Email: email, DBName: dbName}
	if err := s.save(next); err != nil {
		return 0, err
	}
	s.data = next
	return oldID, nil
}

// SetIfAbsent 仅当 telegram_id 不存在、且 email 未被其他 tg_id 占用时写入
// 返回值：写入是否实际发生（false 表示因冲突跳过）
// 注意：本方法不会立即落盘，需在批量调用后由调用方触发 SaveNow
func (s *Store) SetIfAbsent(telegramID int64, email, dbName string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.data[telegramID]; exists {
		return false
	}
	for _, b := range s.data {
		if b.Email == email && b.DBName == dbName {
			return false
		}
	}
	s.data[telegramID] = UserBinding{Email: email, DBName: dbName}
	return true
}

// SaveNow 立即把当前内存状态写入磁盘，用于批量 SetIfAbsent 之后
func (s *Store) SaveNow() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.save(s.data)
}

// Has 判断某个 telegram_id 是否已有本地绑定
func (s *Store) Has(telegramID int64) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, ok := s.data[telegramID]
	return ok
}

func (s *Store) Get(telegramID int64) (UserBinding, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	b, ok := s.data[telegramID]
	return b, ok
}

func (s *Store) Delete(telegramID int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := clone(s.data)
	delete(next, telegramID)
	if err := s.save(next); err != nil {
		return err
	}
	s.data = next
	return nil
}

// FindByEmail 检查某个邮箱是否已被其他 Telegram 账号绑定，返回 telegram_id；未找到返回 0
func (s *Store) FindByEmail(email, dbName string) int64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for tgID, b := range s.data {
		if b.Email == email && b.DBName == dbName {
			return tgID
		}
	}
	return 0
}

// GetAllForDB 获取某个数据库下所有绑定的 telegram_id
func (s *Store) GetAllForDB(dbName string) map[int64]string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make(map[int64]string)
	for tgID, b := range s.data {
		if b.DBName == dbName {
			result[tgID] = b.Email
		}
	}
	return result
}
