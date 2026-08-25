// media.go 入站媒体（图片/文件）处理：从消息 item_list 提取 CDN 引用，
// 下载密文并 AES-128-ECB(PKCS7) 解密。协议来自官方 ClawBot 插件
// （openclaw-weixin）逆向：媒体经 https://novac2c.cdn.weixin.qq.com/c2c
// 中转，key 有三种编码（image_item.aeskey 为 32 位 hex；media.aes_key
// 为 base64，解码后可能是 16 字节裸 key 或 32 位 hex 字符串）。
package weixin

import (
	"context"
	"crypto/aes"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const cdnBaseURL = "https://novac2c.cdn.weixin.qq.com/c2c"

// mediaMaxBytes 单个媒体文件下载上限（与官方插件一致 100MB）。
const mediaMaxBytes = 100 << 20

// cdnRef CDN 媒体引用（CDNMedia）。
type cdnRef struct {
	encryptQueryParam string
	aesKey            string // base64 编码（JSON 原样）
	fullURL           string
}

// inboundMedia 一条入站消息里提取出的媒体。
type inboundMedia struct {
	images   []imageData // 解密后的图片（可直接送多模态模型）
	fileName string      // 文件名（file_item）
	fileData []byte      // 解密后的文件内容
}

type imageData struct {
	mime string
	b64  string
}

// extractMedia 遍历 item_list，下载并解密图片与文件。语音已有服务端
// 转写（voice_item.text），视频暂不支持。下载失败不中断整条消息，记
// 日志后按文本部分继续处理。
func extractMedia(ctx context.Context, msg map[string]any) *inboundMedia {
	items, ok := msg["item_list"].([]any)
	if !ok {
		return nil
	}
	out := &inboundMedia{}
	for _, it := range items {
		item, ok := it.(map[string]any)
		if !ok {
			continue
		}
		typ := 0
		if v := i64p(item["type"]); v != nil {
			typ = int(*v)
		}
		switch typ {
		case 2: // 图片
			ii, _ := item["image_item"].(map[string]any)
			if ii == nil {
				continue
			}
			data, err := fetchImage(ctx, ii)
			if err != nil {
				fmt.Printf("⚠️ 图片下载失败(忽略): %v\n", err)
				continue
			}
			out.images = append(out.images, *data)
		case 4: // 文件
			fi, _ := item["file_item"].(map[string]any)
			if fi == nil {
				continue
			}
			data, err := fetchFile(ctx, fi)
			if err != nil {
				fmt.Printf("⚠️ 文件下载失败(忽略): %v\n", err)
				continue
			}
			out.fileName = str(fi["file_name"])
			out.fileData = data
		}
	}
	if len(out.images) == 0 && out.fileData == nil {
		return nil
	}
	return out
}

// fetchImage 下载并解密 image_item；aeskey 字段（hex）优先于 media.aes_key。
func fetchImage(ctx context.Context, imageItem map[string]any) (*imageData, error) {
	media, _ := imageItem["media"].(map[string]any)
	if media == nil {
		return nil, fmt.Errorf("image_item 缺少 media 字段")
	}
	ref, err := parseCDNRef(media)
	if err != nil {
		return nil, err
	}
	// image_item.aeskey 为 32 位 hex 字符串：转成 base64 走统一解码
	if hexKey := str(imageItem["aeskey"]); isHexKey(hexKey) {
		if raw, herr := hex.DecodeString(hexKey); herr == nil {
			ref.aesKey = base64.StdEncoding.EncodeToString(raw)
		}
	}
	data, err := downloadRef(ctx, ref)
	if err != nil {
		return nil, err
	}
	mime := http.DetectContentType(data)
	if !strings.HasPrefix(mime, "image/") {
		// 解密失败或非图片数据的兜底提示
		return nil, fmt.Errorf("解密后内容不是图片 (%s)", mime)
	}
	return &imageData{mime: mime, b64: base64.StdEncoding.EncodeToString(data)}, nil
}

// fetchFile 下载并解密 file_item，返回文件内容。
func fetchFile(ctx context.Context, fileItem map[string]any) ([]byte, error) {
	media, _ := fileItem["media"].(map[string]any)
	if media == nil {
		return nil, fmt.Errorf("file_item 缺少 media 字段")
	}
	ref, err := parseCDNRef(media)
	if err != nil {
		return nil, err
	}
	return downloadRef(ctx, ref)
}

// parseCDNRef 从 CDNMedia map 提取引用；full_url 与 encrypt_query_param
// 至少要有一个。
func parseCDNRef(media map[string]any) (*cdnRef, error) {
	ref := &cdnRef{
		encryptQueryParam: str(media["encrypt_query_param"]),
		aesKey:            str(media["aes_key"]),
		fullURL:           str(media["full_url"]),
	}
	if ref.fullURL == "" && ref.encryptQueryParam == "" {
		return nil, fmt.Errorf("media 缺少 full_url / encrypt_query_param")
	}
	return ref, nil
}

// downloadRef 下载 CDN 密文并解密；无 key 时按明文下载（部分图片不带 key）。
func downloadRef(ctx context.Context, ref *cdnRef) ([]byte, error) {
	u := ref.fullURL
	if u == "" {
		u = cdnBaseURL + "/download?encrypted_query_param=" + url.QueryEscape(ref.encryptQueryParam)
	}
	ciphertext, err := httpGet(ctx, u)
	if err != nil {
		return nil, fmt.Errorf("CDN 下载: %w", err)
	}
	if ref.aesKey == "" {
		return ciphertext, nil
	}
	key, err := decodeAESKey(ref.aesKey)
	if err != nil {
		return nil, err
	}
	return aesECBDecrypt(ciphertext, key)
}

// decodeAESKey key 解码：base64 → 16 字节裸 key；或 base64 → 32 位 hex
// 字符串 → hex 解码 → 16 字节（文件/语音/视频走这种编码）。
func decodeAESKey(b64 string) ([]byte, error) {
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return nil, fmt.Errorf("aes_key base64 解码: %w", err)
	}
	if len(raw) == 16 {
		return raw, nil
	}
	if len(raw) == 32 && isHexKey(string(raw)) {
		return hex.DecodeString(string(raw))
	}
	return nil, fmt.Errorf("aes_key 解码后为 %d 字节（期望 16 字节或 32 位 hex）", len(raw))
}

// isHexKey 判断是否为 32 位 hex 字符串。
func isHexKey(s string) bool {
	if len(s) != 32 {
		return false
	}
	for _, c := range s {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return true
}

// aesECBDecrypt AES-128-ECB 解密（逐块）并去除 PKCS7 填充。Go 标准库
// 不直接提供 ECB 模式，按 16 字节分块调用 block Decrypt。
func aesECBDecrypt(ciphertext, key []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	bs := block.BlockSize()
	if len(ciphertext) == 0 || len(ciphertext)%bs != 0 {
		return nil, fmt.Errorf("密文长度 %d 不是 %d 的倍数", len(ciphertext), bs)
	}
	out := make([]byte, len(ciphertext))
	for off := 0; off < len(ciphertext); off += bs {
		block.Decrypt(out[off:off+bs], ciphertext[off:off+bs])
	}
	// PKCS7 去填充
	pad := int(out[len(out)-1])
	if pad == 0 || pad > bs || pad > len(out) {
		return nil, fmt.Errorf("非法 PKCS7 填充值 %d", pad)
	}
	for _, b := range out[len(out)-pad:] {
		if int(b) != pad {
			return nil, fmt.Errorf("PKCS7 填充不一致")
		}
	}
	return out[:len(out)-pad], nil
}

// httpGet 简单 GET 下载（带上限）。
func httpGet(ctx context.Context, u string) ([]byte, error) {
	cctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, mediaMaxBytes))
}
