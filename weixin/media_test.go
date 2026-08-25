// media_test.go AES key 解码与 ECB 解密测试（用标准库加密做往返验证）。
package weixin

import (
	"crypto/aes"
	"encoding/base64"
	"encoding/hex"
	"testing"
)

// pkcs7Pad 测试辅助：补 PKCS7 填充。
func pkcs7Pad(data []byte) []byte {
	bs := 16
	pad := bs - len(data)%bs
	out := make([]byte, len(data)+pad)
	copy(out, data)
	for i := len(data); i < len(out); i++ {
		out[i] = byte(pad)
	}
	return out
}

func TestDecodeAESKey(t *testing.T) {
	raw := []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}

	// 格式 A：base64(16 字节裸 key)
	k, err := decodeAESKey(base64.StdEncoding.EncodeToString(raw))
	if err != nil || string(k) != string(raw) {
		t.Errorf("格式 A 解码失败: %v", err)
	}

	// 格式 B：base64(32 位 hex 字符串)
	hexStr := hex.EncodeToString(raw)
	k, err = decodeAESKey(base64.StdEncoding.EncodeToString([]byte(hexStr)))
	if err != nil || string(k) != string(raw) {
		t.Errorf("格式 B 解码失败: %v", err)
	}

	if _, err := decodeAESKey(base64.StdEncoding.EncodeToString([]byte("short"))); err == nil {
		t.Error("非法长度 key 应报错")
	}
}

func TestAESECBDecryptRoundtrip(t *testing.T) {
	key := []byte("0123456789abcdef")
	plain := []byte("微信 ClawBot 媒体解密测试 hello 我们")
	block, _ := aes.NewCipher(key)
	padded := pkcs7Pad(plain)
	ct := make([]byte, len(padded))
	for off := 0; off < len(padded); off += 16 {
		block.Encrypt(ct[off:off+16], padded[off:off+16])
	}
	got, err := aesECBDecrypt(ct, key)
	if err != nil {
		t.Fatalf("解密失败: %v", err)
	}
	if string(got) != string(plain) {
		t.Errorf("往返不一致: %q", got)
	}

	// 篡改填充值应报错而非返回脏数据
	bad := append([]byte(nil), ct...)
	bad[len(bad)-1] = 0xff
	if _, err := aesECBDecrypt(bad, key); err == nil {
		t.Error("非法填充应报错")
	}
}

func TestIsHexKey(t *testing.T) {
	if !isHexKey("00112233445566778899aabbccddeeff") {
		t.Error("合法 hex key 判否")
	}
	if isHexKey("00112233445566778899aabbccddeefg") || isHexKey("abcd") {
		t.Error("非法 hex key 判是")
	}
}
