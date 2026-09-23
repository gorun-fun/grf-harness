package config

// Version 版本号，构建时由 run.sh 通过 -ldflags "-X 本路径" 注入：
//   CI tag 构建取 $CI_COMMIT_TAG（v0.0.1-alpha.4 → 0.0.1-alpha.4），
//   本地构建取 git describe，非 git 环境兜底 dev。
var Version = "dev"
