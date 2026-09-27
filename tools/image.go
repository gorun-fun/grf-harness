// image.go read_image 工具：读取 PNG/JPEG/WebP/GIF 图像并作为多模态
// 内容回传。先按内容解码（PNG/JPEG/GIF 用标准库），mime 一律以解码出
// 的实际格式为准，扩展名仅作准入参考——避免扩展名与内容不符时 mime
// 与字节错位。过大时按比例缩小（最长边 1568）后重编码 PNG；WebP 本地
// 无法解码，确认不可解码的小文件直接透传（base64）。
package tools

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"image"
	"image/color"
	_ "image/gif"
	_ "image/jpeg"
	"image/png"
	"os"
	"path/filepath"
	"strings"
)

const (
	maxImageDim    = 1568 // 最长边上限（与常见 API 建议一致）
	maxPassthrough = 4 << 20
)

// ReadImageFile 读取图像文件，返回 (mime, base64, error)。mime 以实际
// 解码出的格式为准；仅真正的 WebP（本地不可解码）按扩展名透传。
func ReadImageFile(path string) (string, string, error) {
	ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(path), "."))
	switch ext {
	case "png", "jpg", "jpeg", "gif", "webp":
	default:
		return "", "", fmt.Errorf("unsupported image type %q (supported: png/jpg/jpeg/webp/gif)", ext)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", "", err
	}
	if len(data) > 20<<20 {
		return "", "", fmt.Errorf("image file too large (%d bytes)", len(data))
	}

	img, format, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		// 解码失败：仅 .webp 且体积可接受时按 WebP 透传（标准库没有
		// WebP 解码器，真 WebP 必然走到这里；其余扩展名视为损坏图像）
		if ext == "webp" {
			if len(data) > maxPassthrough {
				return "", "", fmt.Errorf("webp file too large to pass through (%d bytes)", len(data))
			}
			return "image/webp", base64.StdEncoding.EncodeToString(data), nil
		}
		return "", "", fmt.Errorf("decode %s image: %w", ext, err)
	}
	b := img.Bounds()
	needsResize := b.Dx() > maxImageDim || b.Dy() > maxImageDim
	if format == "gif" {
		// GIF 取首帧重新编码为 PNG（动画无意义且体积大）
		needsResize = true
	}
	if needsResize {
		img = downscale(img, maxImageDim)
		var buf bytes.Buffer
		if err := png.Encode(&buf, img); err != nil {
			return "", "", err
		}
		return "image/png", base64.StdEncoding.EncodeToString(buf.Bytes()), nil
	}
	// 原样返回的分支也以解码格式定 mime（data 与 format 必然同源）
	switch format {
	case "png":
		return "image/png", base64.StdEncoding.EncodeToString(data), nil
	case "jpeg":
		return "image/jpeg", base64.StdEncoding.EncodeToString(data), nil
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return "", "", err
	}
	return "image/png", base64.StdEncoding.EncodeToString(buf.Bytes()), nil
}

// downscale 等比缩小（最长边 ≤ max；区域平均采样，标准库实现）。
func downscale(img image.Image, maxDim int) image.Image {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	if w <= maxDim && h <= maxDim {
		return img
	}
	scale := float64(maxDim) / float64(max(w, h))
	nw, nh := max(1, int(float64(w)*scale)), max(1, int(float64(h)*scale))
	dst := image.NewRGBA(image.Rect(0, 0, nw, nh))
	sx := float64(w) / float64(nw)
	sy := float64(h) / float64(nh)
	for y := 0; y < nh; y++ {
		y0 := b.Min.Y + int(float64(y)*sy)
		y1 := b.Min.Y + int(float64(y+1)*sy)
		if y1 <= y0 {
			y1 = y0 + 1
		}
		for x := 0; x < nw; x++ {
			x0 := b.Min.X + int(float64(x)*sx)
			x1 := b.Min.X + int(float64(x+1)*sx)
			if x1 <= x0 {
				x1 = x0 + 1
			}
			var rs, gs, bs, as, n uint64
			for yy := y0; yy < y1; yy++ {
				for xx := x0; xx < x1; xx++ {
					r, g, bl, a := img.At(xx, yy).RGBA()
					rs += uint64(r)
					gs += uint64(g)
					bs += uint64(bl)
					as += uint64(a)
					n++
				}
			}
			if n == 0 {
				n = 1
			}
			dst.Set(x, y, color.RGBA{R: uint8(rs / n >> 8), G: uint8(gs / n >> 8), B: uint8(bs / n >> 8), A: uint8(as / n >> 8)})
		}
	}
	return dst
}

// readImageArgs read_image 工具参数（具名类型供 spec 交叉校验测试反射
// json tag）。
type readImageArgs struct {
	FilePath string `json:"file_path"`
}

// RegisterReadImage 注册 read_image 工具。fs 用于读边界校验
// （policy 的 read-workspace 拒绝工作区外路径；可为 nil，跳过校验）。
func RegisterReadImage(r *Registry, fs *FsPolicy) {
	r.Register("read_image", func(ctx context.Context, args string) (Result, error) {
		var a readImageArgs
		if err := decodeArgs(args, &a); err != nil {
			return Result{}, err
		}
		if a.FilePath == "" {
			return Result{}, fmt.Errorf("file_path is required")
		}
		if fs != nil {
			if _, err := fs.CheckRead(a.FilePath); err != nil {
				return Result{}, err
			}
		}
		mime, b64, err := ReadImageFile(a.FilePath)
		if err != nil {
			return Result{}, err
		}
		return Result{
			Text:   fmt.Sprintf("image %s (%s) loaded and will be attached to your next context as an image part; describe/analyze it directly", a.FilePath, mime),
			Images: []Image{{Mime: mime, B64: b64}},
		}, nil
	})
}
