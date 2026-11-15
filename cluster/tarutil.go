// tarutil.go 集群文件传输的打包/解包（M2）：归档格式 tar.gz，纯标准库。
// PackPaths 在发送侧打包（可挂 FsPolicy.CheckRead 读边界）；UnpackTo 在
// 接收侧安全解包——拒绝绝对路径/.. 穿越、拒绝 symlink/hardlink 与设备
// 条目（只收目录+常规文件），设解压总量与文件数上限，防压缩炸弹与
// 穿越写入。两侧上限与 ShareMaxBytes 同族（打包 ≤ 传输 ≤ 解压）。
package cluster

import (
	"archive/tar"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"git.gorun.fun/share/grf-harness/tools"
)

// 打包/解包护栏。
const (
	packMaxFiles   = 10000
	packMaxBytes   = ShareMaxBytes // 与单对象传输上限一致（打包即终检）
	unpackMaxBytes = 512 << 20     // 解压总量上限（> 传输上限，容忍合法的接收侧重复收包）
	unpackMaxFiles = 10000
)

// PackPaths 把 root 下的一组路径打成 tar.gz 写入 w，返回收录的文件清单
// （相对 root、字典序——产物 manifest / 种子比对共用）。paths 相对 root
// （也接受 root 内的绝对路径，归一为相对名）；目录递归收录。fsp 非空时
// 逐入口路径过 CheckRead（发送侧读边界，硬拒即败）。
func PackPaths(fsp *tools.FsPolicy, root string, paths []string, w io.Writer) ([]string, error) {
	// 1. 展开入口为文件清单（先收集再写头，超限早败不产半包）
	var files []string // 相对 root 的归档名
	for _, p := range paths {
		abs := p
		if !filepath.IsAbs(abs) {
			abs = filepath.Join(root, p)
		}
		if fsp != nil {
			if _, err := fsp.CheckRead(abs); err != nil {
				return nil, err
			}
		}
		fi, err := os.Stat(abs)
		if err != nil {
			return nil, fmt.Errorf("cluster pack: %w", err)
		}
		if fi.IsDir() {
			err = filepath.WalkDir(abs, func(path string, d os.DirEntry, err error) error {
				if err != nil {
					return err
				}
				if d.Type().IsRegular() {
					rel, ok := relInside(root, path)
					if !ok {
						return fmt.Errorf("cluster pack: %q escapes root %q", path, root)
					}
					files = append(files, rel)
				}
				return nil
			})
			if err != nil {
				return nil, fmt.Errorf("cluster pack: %w", err)
			}
		} else {
			rel, ok := relInside(root, abs)
			if !ok {
				return nil, fmt.Errorf("cluster pack: %q escapes root %q", abs, root)
			}
			files = append(files, rel)
		}
	}
	sort.Strings(files)
	// 去重：入口/目录展开可能重叠，重复条目会产生重复归档项
	if len(files) > 1 {
		uniq := files[:1]
		for _, f := range files[1:] {
			if f != uniq[len(uniq)-1] {
				uniq = append(uniq, f)
			}
		}
		files = uniq
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("cluster pack: nothing to pack under %q from %v", root, paths)
	}
	if len(files) > packMaxFiles {
		return nil, fmt.Errorf("cluster pack: %d files exceeds limit %d", len(files), packMaxFiles)
	}

	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)
	total := int64(0)
	for _, rel := range files {
		abs := filepath.Join(root, rel)
		fi, err := os.Stat(abs)
		if err != nil {
			return nil, fmt.Errorf("cluster pack: %w", err)
		}
		hdr := &tar.Header{
			Name:     filepath.ToSlash(rel),
			Mode:     tarMode(fi),
			Size:     fi.Size(),
			Typeflag: tar.TypeReg,
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return nil, fmt.Errorf("cluster pack: %w", err)
		}
		f, err := os.Open(abs)
		if err != nil {
			return nil, fmt.Errorf("cluster pack: %w", err)
		}
		n, err := io.Copy(tw, f)
		f.Close()
		if err != nil {
			return nil, fmt.Errorf("cluster pack: %w", err)
		}
		total += n
		if total > packMaxBytes {
			return nil, fmt.Errorf("cluster pack: archive exceeds %d bytes limit", packMaxBytes)
		}
	}
	if err := tw.Close(); err != nil {
		return nil, fmt.Errorf("cluster pack: %w", err)
	}
	if err := gz.Close(); err != nil {
		return nil, fmt.Errorf("cluster pack: %w", err)
	}
	return files, nil
}

// relInside root 内路径 → 相对名；越出 root（符号链接展开后经 WalkDir
// 实路径落入区外等）返回 false，由调用方报错。路径分隔符归一为 /。
func relInside(root, path string) (string, bool) {
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	return rel, true
}

// tarMode 归档条目权限：只保留可执行位差异（收包侧统一 0644/0755）。
func tarMode(fi os.FileInfo) int64 {
	if fi.Mode()&0o100 != 0 {
		return 0o755
	}
	return 0o644
}

// UnpackTo 把 tar.gz 流安全解包到 dest（必须已是空目录或可写目录）。
// 只接受目录与常规文件条目；条目名拒绝绝对路径、.. 分量与反斜杠；
// 总量/条目数超限即败（已写出的部分由调用方负责丢弃整个 dest）。
func UnpackTo(r io.Reader, dest string) (int, error) {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return 0, fmt.Errorf("cluster unpack: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	count := 0
	total := int64(0)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return count, fmt.Errorf("cluster unpack: %w", err)
		}
		rel, err := safeRelName(hdr.Name)
		if err != nil {
			return count, err
		}
		target := filepath.Join(dest, rel)
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return count, fmt.Errorf("cluster unpack: %w", err)
			}
		case tar.TypeReg:
			if count >= unpackMaxFiles {
				return count, fmt.Errorf("cluster unpack: more than %d entries", unpackMaxFiles)
			}
			if total+hdr.Size > unpackMaxBytes {
				return count, fmt.Errorf("cluster unpack: archive expands beyond %d bytes", unpackMaxBytes)
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return count, fmt.Errorf("cluster unpack: %w", err)
			}
			if err := writeFileTracked(target, tr, hdr); err != nil {
				return count, fmt.Errorf("cluster unpack: %w", err)
			}
			count++
			total += hdr.Size
		default:
			return count, fmt.Errorf("cluster unpack: entry %q has unsupported type %q (only dirs and regular files are allowed for security)", hdr.Name, string(rune(hdr.Typeflag)))
		}
	}
	if count == 0 {
		return 0, fmt.Errorf("cluster unpack: archive contains no regular files")
	}
	return count, nil
}

// writeFileTracked 按头权限（0644/0755 收窄）落一个常规文件。
func writeFileTracked(target string, r io.Reader, hdr *tar.Header) error {
	mode := os.FileMode(0o644)
	if hdr.Mode != 0o644 {
		mode = 0o755
	}
	f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	// 头声明大小即写多少（tar.Reader 已按头截断，不会越写）
	if _, err := io.CopyN(f, r, hdr.Size); err != nil && err != io.EOF {
		f.Close()
		return err
	}
	return f.Close()
}

// safeRelName 校验归档条目名并返回平台相对路径：拒绝绝对路径、..
// 分量、反斜杠与空名（绝对路径不洗白——发送侧 bug 或恶意构造一律拒收）。
func safeRelName(name string) (string, error) {
	if name == "" || strings.Contains(name, "\\") || strings.HasPrefix(name, "/") {
		return "", fmt.Errorf("cluster unpack: illegal entry name %q", name)
	}
	rel := filepath.Clean(filepath.FromSlash(name))
	if filepath.IsAbs(rel) || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || rel == ".." || rel == "." {
		return "", fmt.Errorf("cluster unpack: illegal entry name %q", name)
	}
	return rel, nil
}
