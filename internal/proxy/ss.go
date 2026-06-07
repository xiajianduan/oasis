package proxy

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/md5"
	"crypto/rand"
	"fmt"
	"io"
	"net"
	"time"

	"golang.org/x/crypto/chacha20poly1305"
)

// StreamCipher 流加密接口
type StreamCipher interface {
	Encrypt(dst, src []byte)
	Decrypt(dst, src []byte)
	KeySize() int
	SaltSize() int
}

// NewStreamCipher 根据方法创建加密器
func NewStreamCipher(method, password string) (StreamCipher, error) {
	switch method {
	case "aes-256-gcm":
		return &aeadCipher{method: method, keySize: 32, saltSize: 16}, nil
	case "aes-128-gcm":
		return &aeadCipher{method: method, keySize: 16, saltSize: 16}, nil
	case "chacha20-ietf-poly1305":
		return &aeadCipher{method: method, keySize: 32, saltSize: 16}, nil
	default:
		return nil, fmt.Errorf("不支持的加密方法: %s", method)
	}
}

type aeadCipher struct {
	method   string
	keySize  int
	saltSize int
}

func (c *aeadCipher) KeySize() int  { return c.keySize }
func (c *aeadCipher) SaltSize() int { return c.saltSize }

func (c *aeadCipher) Encrypt(dst, src []byte) {
	panic("aeadCipher.Encrypt: 请使用 NewSSConn 进行加密传输")
}

func (c *aeadCipher) Decrypt(dst, src []byte) {
	panic("aeadCipher.Decrypt: 请使用 NewSSConn 进行解密传输")
}

// SSConn Shadowsocks 加密连接
type SSConn struct {
	conn     net.Conn
	cipher   StreamCipher
	password string

	// AEAD 状态
	key      []byte
	salt     []byte
	enc      cipher.AEAD
	dec      cipher.AEAD
	encNonce []byte
	decNonce []byte
}

// NewSSConn 创建 Shadowsocks 加密连接
func NewSSConn(conn net.Conn, sc StreamCipher, password string) *SSConn {
	return &SSConn{
		conn:     conn,
		cipher:   sc,
		password: password,
	}
}

func (s *SSConn) initAEAD(password string, salt []byte) error {
	key := kdf(password, salt, s.cipher.KeySize())

	var aead cipher.AEAD
	var err error

	switch s.cipher.(*aeadCipher).method {
	case "aes-256-gcm", "aes-128-gcm":
		block, err := aes.NewCipher(key)
		if err != nil {
			return err
		}
		aead, err = cipher.NewGCM(block)
		if err != nil {
			return err
		}
	case "chacha20-ietf-poly1305":
		aead, err = chacha20poly1305.New(key)
		if err != nil {
			return err
		}
	}

	s.key = key
	s.salt = salt
	s.enc = aead
	s.dec = aead
	s.encNonce = make([]byte, aead.NonceSize())
	s.decNonce = make([]byte, aead.NonceSize())

	return nil
}

func (s *SSConn) initDecrypt(password string) error {
	salt := make([]byte, s.cipher.SaltSize())
	if _, err := io.ReadFull(s.conn, salt); err != nil {
		return fmt.Errorf("读取 SS salt 失败: %w", err)
	}
	return s.initAEAD(password, salt)
}

// Read 解密读取
func (s *SSConn) Read(b []byte) (int, error) {
	if s.dec == nil {
		if err := s.initDecrypt(s.password); err != nil {
			return 0, err
		}
	}

	// 读取长度 (2 字节 + tag)
	lenBuf := make([]byte, 2+s.dec.Overhead())
	if _, err := io.ReadFull(s.conn, lenBuf); err != nil {
		return 0, err
	}

	// 解密长度
	plainLen, err := s.dec.Open(lenBuf[:0], s.decNonce, lenBuf, nil)
	if err != nil {
		return 0, fmt.Errorf("SS 解密长度失败: %w", err)
	}
	incrementNonce(s.decNonce)

	length := (int(plainLen[0]) << 8) | int(plainLen[1])
	if length == 0 {
		return 0, nil
	}
	if length > len(b) {
		length = len(b)
	}

	// 读取密文
	payloadLen := length + s.dec.Overhead()
	payload := make([]byte, payloadLen)
	if _, err := io.ReadFull(s.conn, payload); err != nil {
		return 0, err
	}

	// 解密
	plain, err := s.dec.Open(b[:0], s.decNonce, payload, nil)
	if err != nil {
		return 0, fmt.Errorf("SS 解密失败: %w", err)
	}
	incrementNonce(s.decNonce)

	return len(plain), nil
}

// Write 加密写入
func (s *SSConn) Write(b []byte) (int, error) {
	if s.enc == nil {
		salt := make([]byte, s.cipher.SaltSize())
		if _, err := rand.Read(salt); err != nil {
			return 0, err
		}
		if _, err := s.conn.Write(salt); err != nil {
			return 0, err
		}
		if err := s.initAEAD(s.password, salt); err != nil {
			return 0, err
		}
	}

	total := 0
	for len(b) > 0 {
		chunk := b
		if len(chunk) > 0x3FFF {
			chunk = chunk[:0x3FFF]
		}
		b = b[len(chunk):]

		// 加密长度
		lenBuf := []byte{byte(len(chunk) >> 8), byte(len(chunk))}
		encLen := s.enc.Seal(lenBuf[:0], s.encNonce, lenBuf, nil)
		if _, err := s.conn.Write(encLen); err != nil {
			return total, err
		}
		incrementNonce(s.encNonce)

		// 加密数据
		encData := s.enc.Seal(chunk[:0], s.encNonce, chunk, nil)
		if _, err := s.conn.Write(encData); err != nil {
			return total, err
		}
		incrementNonce(s.encNonce)

		total += len(chunk)
	}

	return total, nil
}

func (s *SSConn) Close() error {
	return s.conn.Close()
}

func (s *SSConn) LocalAddr() net.Addr {
	return s.conn.LocalAddr()
}

func (s *SSConn) RemoteAddr() net.Addr {
	return s.conn.RemoteAddr()
}

func (s *SSConn) SetDeadline(t time.Time) error {
	return s.conn.SetDeadline(t)
}

func (s *SSConn) SetReadDeadline(t time.Time) error {
	return s.conn.SetReadDeadline(t)
}

func (s *SSConn) SetWriteDeadline(t time.Time) error {
	return s.conn.SetWriteDeadline(t)
}

func incrementNonce(nonce []byte) {
	for i := len(nonce) - 1; i >= 0; i-- {
		nonce[i]++
		if nonce[i] != 0 {
			break
		}
	}
}

// kdf Shadowsocks 密钥派生函数
func kdf(password string, salt []byte, keyLen int) []byte {
	// 简化版 EVP_BytesToKey
	key := make([]byte, 0, keyLen)
	prev := make([]byte, 0)

	for len(key) < keyLen {
		h := md5.New()
		h.Write(prev)
		h.Write([]byte(password))
		h.Write(salt)
		prev = h.Sum(nil)
		key = append(key, prev...)
	}

	return key[:keyLen]
}


