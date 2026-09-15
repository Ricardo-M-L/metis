#!/usr/bin/env bash
# 本地阅读样例：仅向标准输出打印报告，不写文件，不访问网络。
set -euo pipefail

report_title="文件预览体验验收"
report_date="2026-09-14"
report_owner="交付小组"
sample_files=("验收报告.md" "regen.sh" "worker.go" "notes.txt")

print_header() {
  printf '# %s\n\n' "$report_title"
  printf '日期：%s · 负责人：%s\n\n' "$report_date" "$report_owner"
}

print_files() {
  printf '## 交付文件\n\n'
  for sample_file in "${sample_files[@]}"; do
    printf -- '- %s\n' "$sample_file"
  done
  printf '\n'
}

print_checklist() {
  printf '## 阅读检查\n\n'
  printf -- '- [x] 核对文件名称和类型\n'
  printf -- '- [x] 保留中文标题与段落\n'
  printf -- '- [ ] 复核窄窗口中的长代码行\n'
  printf '\n说明：以上状态仅为演示数据。\n'
}

main() {
  print_header
  print_files
  print_checklist
}

main "$@"
