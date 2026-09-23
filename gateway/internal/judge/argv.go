package judge

import "strings"

// 本文件是「命令字符串 → argv」的确定性分词，供规则后端与采集侧共用。
//
// 为什么放在判断层而不是采集侧：分词是纯函数、无副作用，而它同时被
//   - 采集侧（proxy / mcp）用来填 ActionDescriptor.Argv，
//   - 规则后端用来在**直接构造** ActionDescriptor 的场合（测试、/v1/judge 端点）兜底
// 使用。放在一处，两边不会有实现分歧——分歧会导致「同一条命令在采集侧和
// 判定侧被看成两个动作」这类难查的问题。
//
// 明确不支持：变量展开（`$VAR`）、命令替换（`$(...)`）、通配符展开、重定向语义。
// 这些都属于「求值」而非「分词」，做进来越权且不可靠。规则后端把无法求值的
// 形态当作「疑似混淆」处理（见 rules.go），交给人或后续后端。

// SplitCommands 把一条 shell 命令行切成若干「简单命令」的 argv 序列。
//
// 分隔符：换行、`;`、`&&`、`||`、`|`、重定向符。引号与反斜杠转义按 shell 常规处理。
//
//	"t=tar; t -czf x.tar.gz ."  →  [["t=tar"], ["t","-czf","x.tar.gz","."]]
//	`tar -czf "my repo.tar.gz" .` → [["tar","-czf","my repo.tar.gz","."]]
func SplitCommands(cmd string) [][]string {
	var out [][]string
	var cur []string
	var tok strings.Builder
	inTok := false
	var quote byte

	flushTok := func() {
		if inTok {
			cur = append(cur, tok.String())
			tok.Reset()
			inTok = false
		}
	}
	flushCmd := func() {
		flushTok()
		if len(cur) > 0 {
			out = append(out, cur)
			cur = nil
		}
	}

	for i := 0; i < len(cmd); i++ {
		c := cmd[i]
		if quote != 0 {
			switch {
			case c == quote:
				quote = 0
			case c == '\\' && quote == '"' && i+1 < len(cmd):
				i++
				tok.WriteByte(cmd[i])
			default:
				tok.WriteByte(c)
			}
			inTok = true
			continue
		}
		switch c {
		case '\'', '"':
			quote = c
			// 空引号 `""` 也是一个（空）参数，置位以保证被 flush 出来。
			inTok = true
		case '\\':
			if i+1 < len(cmd) {
				i++
				tok.WriteByte(cmd[i])
			}
			inTok = true
		case ' ', '\t', '\r':
			flushTok()
		case '\n', ';':
			flushCmd()
		case '&', '|':
			flushCmd()
			if i+1 < len(cmd) && cmd[i+1] == c {
				i++ // `&&` / `||`
			}
		case '>', '<':
			flushCmd()
			if i+1 < len(cmd) && (cmd[i+1] == c || cmd[i+1] == '&') {
				i++ // `>>` / `>&`
			}
		default:
			tok.WriteByte(c)
			inTok = true
		}
	}
	flushCmd()
	return out
}

// SplitCommand 只取第一条简单命令的 argv（最常见的用法）。
func SplitCommand(cmd string) []string {
	cmds := SplitCommands(cmd)
	if len(cmds) == 0 {
		return nil
	}
	return cmds[0]
}

// maxUnwrapDepth `sh -c "sh -c ..."` 的展开深度上限。嵌套壳命令是真实存在的，
// 但不值得为它做无界递归——超过深度就当作「判不了」交给上层。
const maxUnwrapDepth = 2

// cmdWrappers 只改执行环境、不改动作语义的前缀命令。
//
// 不放进来的：`timeout 5 cmd`（带位置参数）、`xargs cmd`（输入来自管道）。
// 这两类剥离规则复杂且容易剥错，不如让它们落到「文本里有工具名但首词对不上」
// 那条 unknown 分支上——诚实地说判不了，比猜错安全。
var cmdWrappers = map[string]bool{
	"sudo": true, "doas": true, "env": true, "nice": true,
	"nohup": true, "ionice": true, "setsid": true,
	"sh": true, "bash": true, "zsh": true, "dash": true, "busybox": true,
}

// ExpandedCommands 在 SplitCommands 基础上剥掉 shell 包装、展开 `sh -c "..."`。
//
// 目的：`bash -c "tar -czf repo.tar.gz ."` 与 `sudo tar -czf repo.tar.gz .` 是
// 同一件事的两种常见写法，规则后端必须看到内层的 `tar`——否则最典型的
// 「让 agent 打包整仓」形态会被漏掉。
func ExpandedCommands(cmd string) [][]string {
	out := make([][]string, 0, 4)
	var walk func(s string, depth int)
	walk = func(s string, depth int) {
		for _, argv := range SplitCommands(s) {
			if len(argv) == 0 {
				continue
			}
			inner, rest := peelWrappers(argv)
			if inner != "" && depth < maxUnwrapDepth {
				walk(inner, depth+1)
				continue
			}
			if len(rest) > 0 {
				out = append(out, rest)
			}
		}
	}
	walk(cmd, 0)
	return out
}

// peelWrappers 剥掉前导 wrapper。
//
// 返回 (内层命令字符串, 剩余 argv)：
//   - 命中 `sh -c "..."` → 返回内层字符串，由调用方递归；
//   - 只是 `sudo cmd ...` → 返回剥掉前缀后的 argv；
//   - 没有可剥的 → rest 即原 argv。
func peelWrappers(argv []string) (inner string, rest []string) {
	i := 0
	for i < len(argv) {
		if !cmdWrappers[strings.ToLower(argv[i])] {
			break
		}
		i++
		for i < len(argv) && strings.HasPrefix(argv[i], "-") {
			if argv[i] == "-c" || argv[i] == "--command" {
				if i+1 < len(argv) {
					return argv[i+1], nil
				}
				return "", nil
			}
			i++
		}
		// `env FOO=bar cmd` —— 跳过环境变量赋值。
		for i < len(argv) && isAssignment(argv[i]) {
			i++
		}
	}
	return "", argv[i:]
}

// isIdentChar 环境变量名允许的字符：ASCII 字母、数字、下划线。
//
// 刻意不用 unicode.IsLetter —— 它接受非 ASCII 字母，会把 `名字=值` 也认成赋值。
// shell 的变量名是窄字符集，这里跟着窄。
func isIdentChar(r rune) bool {
	return r == '_' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9'
}

// isAssignment 该 token 是否是 `NAME=value` 形态（且不是 URL 或选项）。
func isAssignment(tok string) bool {
	if tok == "" || strings.HasPrefix(tok, "-") {
		return false
	}
	i := strings.IndexByte(tok, '=')
	if i <= 0 {
		return false
	}
	name := tok[:i]
	if strings.Contains(name, "://") {
		return false
	}
	for _, r := range name {
		if !isIdentChar(r) {
			return false
		}
	}
	return true
}

// FirstWord 命令的「首词」——采集侧用它填 ActionDescriptor.Target。
//
// 变量引用（`$t`）与赋值（`t=tar`）都会原样返回：规则后端需要看到它们
// 才能识别「间接引用」这类混淆形态，提前展开反而把信号抹掉了。
func FirstWord(cmd string) string {
	argv := SplitCommand(cmd)
	if len(argv) == 0 {
		return ""
	}
	return argv[0]
}

// HasShellIndirection 命令行里是否存在无法静态求值的间接引用。
//
// 命中说明「规则只能看到字面量」，因此对这类输入应当降级（unknown）或降 severity，
// 而不是自信地判 benign——`t=tar; $t -czf .` 这种写法的字面量里没有 tar 命令，
// 但它确实在打包。
func HasShellIndirection(cmds [][]string) bool {
	for _, argv := range cmds {
		if len(argv) == 0 {
			continue
		}
		head := argv[0]
		if strings.ContainsAny(head, "$`") {
			return true
		}
		if strings.Contains(head, "=") && !strings.HasPrefix(head, "-") && !strings.Contains(head, "://") {
			return true // 形如 NAME=value 的赋值
		}
		if head == "eval" || head == "exec" || head == "source" || head == "." {
			return true
		}
	}
	return false
}

// Assignments 返回命令序列里所有 `NAME=value` 赋值的值部分（小写化前的原文）。
//
// 用途：`t=tar; $t -czf .` 里 tar 只出现在赋值中，规则需要把它捞出来看。
func Assignments(cmds [][]string) []string {
	var out []string
	for _, argv := range cmds {
		if len(argv) == 0 {
			continue
		}
		head := argv[0]
		if strings.HasPrefix(head, "-") {
			continue
		}
		if i := strings.IndexByte(head, '='); i > 0 && !strings.Contains(head[:i], ":") {
			out = append(out, head[i+1:])
		}
	}
	return out
}
