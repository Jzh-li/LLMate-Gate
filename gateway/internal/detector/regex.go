package detector

import (
	"context"
	"regexp"
	"strings"
	"time"

	"gateway/pkg/cn"
	"gateway/pkg/global"
	"gateway/pkg/types"
)

// RegexEngine 内置中文正则检测引擎。
//
// 定位（决策 D001）：零依赖、开箱可跑，用于「格式固定实体」的高精度检测
// ——手机号、身份证、银行卡、邮箱、IP、日期、车牌、密钥等。
// 弱格式实体（中文人名、地址）只做**高精度低召回**的启发式检测，
// 完整召回依赖 detection.engine=pii-engineer 的 NER 模型。
type RegexEngine struct {
	opts  *options
	rules []rule
	// shapeRules 是只在「归一化窗口」上跑的形态专用规则（国家码 / 点分号码）。
	shapeRules []rule
	// windowRules 是窗口那一遍要跑的规则全集 = rules 里形态敏感的通用规则 + shapeRules。
	// 与 rules 同源（构造时按类型过滤），保证两条路径的判定不会各写一份。
	windowRules []rule
}

type rule struct {
	entityType string
	score      float64
	re         *regexp.Regexp
	// validate 返回 (entityType, ok)：
	//   - entityType 为空时回退到 rule.entityType；
	//   - ok=false 时跳过该匹配；
	//   - 部分规则（银行卡/信用卡 dispatch）通过返回值动态指定 type。
	validate func(string) (string, bool)
	// digitBoundary 为 true 时，要求匹配左右邻字符不是数字（RE2 不支持 lookaround，
	// 因此用手工边界检查替代 (?<!\d)/(?!\d)）。
	digitBoundary bool
	// idBoundary 为 true 时，要求左右邻字符不是数字或 X/x（身份证场景）。
	idBoundary bool
}

var (
	rePhone    = regexp.MustCompile(`1[3-9][0-9]{9}`)
	reIDCard18 = regexp.MustCompile(`[1-9][0-9]{5}(?:19|20)[0-9]{2}(?:0[1-9]|1[0-2])(?:0[1-9]|[12][0-9]|3[01])[0-9]{3}[0-9Xx]`)
	reIDCard15 = regexp.MustCompile(`[1-9][0-9]{7}(?:0[1-9]|1[0-2])(?:0[1-9]|[12][0-9]|3[01])[0-9]{3}`)
	// 国际信用卡（Visa/MC/Amex/Disc/JCB/UnionPay）：
	// Visa 13/16/19, MasterCard 16, Amex 15, Disc 16, JCB 15-16, UPI 16-19。
	// 范围 13-19；zh_bank_card / credit_card 的区分由 cardDispatch 按 IIN 前缀 + Luhn 动态裁决。
	reCreditCard = regexp.MustCompile(`[0-9]{13,19}`)
	reEmail      = regexp.MustCompile(`[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}`)
	reIPv4       = regexp.MustCompile(`(?:(?:25[0-5]|2[0-4][0-9]|1[0-9]{2}|[1-9]?[0-9])\.){3}(?:25[0-5]|2[0-4][0-9]|1[0-9]{2}|[1-9]?[0-9])`)
	// 日期。日部分的交替**必须长分支优先**。
	//
	// Go regexp 取 leftmost-first：按交替的书写顺序返回**最先到达接受态**的那一支，
	// 不再尝试后面的分支。日后面跟的是**可选**的 `日?`，所以短分支匹配到第一位数字时
	// 就已经是接受态 —— `2024-09-17` 被截成 `2024-09-1`，替换后原文残留一个 `7`
	// （`2024年12月31日` 更差：残留 `1日`）。日 ≥ 10 的日期全部中招，
	// 实测 10 个书写形态里 8 个被截断。
	//
	// 月部分同样是短优先，但后面跟的是**必需**的 `[-/.月]` —— 短分支会导致整体失败，
	// 引擎回退去试长分支，因而月一直是对的（`reIDCard18` / `reIDCard15` 的日同理）。
	// 也就是说：**判断这种排列会不会出错，看交替后面跟的是必需元素还是可选元素**，
	// 不是看写法本身。
	//
	// 重排无实质性能代价：RE2 是 NFA 优先级模拟而非回溯引擎，改的只是分支优先顺序。
	reDate = regexp.MustCompile(`(?:19|20)[0-9]{2}[-/.年](?:0?[1-9]|1[0-2])[-/.月](?:3[01]|[12][0-9]|0?[1-9])日?`)
	// 【2026-09-11 决议】接线：types.EntityPlate = "plate"（中英统一，pkg/types §6.1 注脚）。
	// 冲突标注撤销：原"未接线"问题已通过新增 pkg/types.EntityPlate + rePlate/rePlateEN/rePlateCA
	// 三正则（中文严格 / 英文 / 加州）解决，bench 验证 F1=1.0。
	//
	// 【2026-09-11 拍板】接线：types.EntityPlate = "plate"（中英统一，pkg/types §6.1 注脚）。
	rePlate = regexp.MustCompile(`[京津沪渝冀豫云辽黑湘皖鲁新苏浙赣鄂桂甘晋蒙陕吉闽贵粤青藏川宁琼][A-Z][A-HJ-NP-Z0-9]{4,6}`)
	// 英文车牌（美国各州格式不一，无校验位）——弱格式实体。
	// 处理方式与中文人名一致（reNameCtx 模式）：强上下文 + 只报子匹配：
	//   1. 必须有 "license/vehicle registration plate (number):" 引导词；
	//   2. 车牌本体 = 2-3 大写字母 + 可选连字符/空格 + 3-4 数字 + 可选尾字母，
	//      例如 "License plate: ABC-1234" / "CA-1234" / "7XWA123"（加州 digit-first 另配）。
	//   3. 纯字母缩写（API/HTTP/ISO）不命中——需要至少 3 位数字。
	// 加州 digit-first 格式（7XWA123）：单独一条子规则。
	rePlateEN = regexp.MustCompile(`(?i:\b(?:license|vehicle|registration)\s+plate(?:\s+number)?\s*[:#]?\s*)([A-Z]{2,3}[-]?[0-9]{3,4}[A-Z]?)\b`)
	rePlateCA = regexp.MustCompile(`(?i:\b(?:license|vehicle|registration)\s+plate(?:\s+number)?\s*[:#]?\s*)([0-9][A-Z]{3}[0-9]{3})\b`)
	// URL：http/https/ftp://...，最后一个字符不允许是句尾标点（. , ; : ! ? ) ] }），
	// 避免 "Visit https://x.com." 把句号算进 URL。
	// 实现：主体 [^...]+ + 结尾 [A-Za-z0-9/~#=&_+]（RE2 leftmost-first 正确处理回让）。
	reURL = regexp.MustCompile(`\bhttps?://[^\s<>"'{}\\^\x60]+[A-Za-z0-9/~#=&_\-+]|\bftp://[^\s<>"'{}\\^\x60]+[A-Za-z0-9/~#=&_\-+]`)
	// 美国 SSN：3-2-4 形态（带分隔符），见 global.ValidUSSSN 的过滤逻辑。
	reUSSSN = regexp.MustCompile(`\b\d{3}-\d{2}-\d{4}\b`)
	// 【2026-09-21 形态容忍】国家码手机号：`+86-186-1234-5678` / `+86 186 1234 5678`
	// / `+8618612345678`。国家码本身进 span —— 它同样是可识别到人的信息的一部分，
	// 且真对抗日志的 GT 就是这么标的（整串 `+86-186-1234-5678`）。
	//
	// 为什么这条规则自带分隔符容忍、而不交给归一化：归一化只在**文本确实需要归一化时**
	// 才触发，`+8618612345678` 这种本来就干净的写法不会触发，若不在此容错就会漏掉。
	// 只有 `-` 与空格两种分隔符 —— `\s` 会把换行也算进去，可能跨行拼接，不值得。
	rePhoneCC = regexp.MustCompile(`(?:\+|00)?86[ \-]?1[3-9](?:[ \-]?\d){9}`)
	// 点分手机号（3-4-4 分组），例如 `138.0013.8000`。
	//
	// 为什么能在不伤 IPv4 的前提下加这条：3-4-4 的分组里存在**四位组**，而合法 IPv4
	// 的每一组都不超过 255（至多三位），所以本规则与 reIPv4 天然互斥。
	// 这也是归一化里宁可保留 `.` 的代价所在（见 normalize.go 文件头第 2 条）。
	rePhoneDotted = regexp.MustCompile(`1[3-9][0-9]\.[0-9]{4}\.[0-9]{4}`)
	reAPIKey      = regexp.MustCompile(`\b(?:sk|pk|api|ak)-[A-Za-z0-9_\-]{16,}`)
	reJWT         = regexp.MustCompile(`\beyJ[A-Za-z0-9_\-]{8,}\.[A-Za-z0-9_\-]{8,}\.[A-Za-z0-9_\-]{8,}`)
	reSecretKV    = regexp.MustCompile(`(?i)\b(?:api[_\-]?key|secret|password|passwd|pwd|token|access[_\-]?key)\b\s*[:=]\s*["']?([A-Za-z0-9_\-\.]{8,})["']?`)
	// 高精度人名：必须有强上下文引导词。弱格式实体在正则引擎下只保证精确率。
	reNameCtx  = regexp.MustCompile(`(?:我叫|姓名|名字是|客户|联系人|收件人|负责人|申请人|用户|病人|患者|就诊人|员工|同学|本人|我是)\s*[:：是为]?\s*([一-龥]{2,4})`)
	reNameTile = regexp.MustCompile(`([一-龥]{2,4})(?:先生|女士|小姐|老师|医生|同学|工程师|经理|总监)`)
	// 中文地址：要求出现行政区划关键词以降低误报。
	// 顶部行政区划可以是 X省/自治区，也可以是直辖市（北京/上海/天津/重庆）——
	// 直辖市没有省级前缀，必须单独列出，否则漏检（baseline 0.94 → 改进后 ~0.99）。
	reAddress = regexp.MustCompile(
		`(?:[一-龥]{2,8}(?:省|自治区)|(?:北京市|上海市|天津市|重庆市))` +
			`[一-龥]{2,12}(?:市|区|县|旗|盟)` +
			`[一-龥]{2,20}(?:路|街|道|大街|大道|胡同|里|弄|巷|村|镇|园区|工业园)` +
			`[0-9]*[号院栋楼室单元层]*`,
	)
)

// 常见中文姓氏，用于剔除人名启发式的明显误报（如"我们"、"公司"）。
var commonSurnames = map[string]bool{
	"赵": true, "钱": true, "孙": true, "李": true, "周": true, "吴": true, "郑": true, "王": true,
	"冯": true, "陈": true, "褚": true, "卫": true, "蒋": true, "沈": true, "韩": true, "杨": true,
	"朱": true, "秦": true, "尤": true, "许": true, "何": true, "吕": true, "施": true, "张": true,
	"孔": true, "曹": true, "严": true, "华": true, "金": true, "魏": true, "陶": true, "姜": true,
	"戚": true, "谢": true, "邹": true, "喻": true, "柏": true, "水": true, "窦": true, "章": true,
	"云": true, "苏": true, "潘": true, "葛": true, "奚": true, "范": true, "彭": true, "郎": true,
	"鲁": true, "韦": true, "昌": true, "马": true, "苗": true, "凤": true, "花": true, "方": true,
	"俞": true, "任": true, "袁": true, "柳": true, "唐": true, "罗": true, "薛": true, "雷": true,
	"贺": true, "倪": true, "汤": true, "滕": true, "殷": true, "毕": true, "郝": true, "邬": true,
	"安": true, "常": true, "乐": true, "于": true, "时": true, "傅": true, "皮": true, "齐": true,
	"康": true, "伍": true, "余": true, "元": true, "卜": true, "顾": true, "孟": true, "平": true,
	"黄": true, "和": true, "穆": true, "萧": true, "尹": true, "姚": true, "邵": true, "湛": true,
	"汪": true, "祁": true, "毛": true, "禹": true, "狄": true, "米": true, "贝": true, "明": true,
	"臧": true, "计": true, "伏": true, "成": true, "戴": true, "谈": true, "宋": true, "茅": true,
	"庞": true, "熊": true, "纪": true, "舒": true, "屈": true, "项": true, "祝": true, "董": true,
	"梁": true, "杜": true, "阮": true, "蓝": true, "闵": true, "席": true, "季": true, "麻": true,
	"强": true, "贾": true, "路": true, "娄": true, "危": true, "江": true, "童": true, "颜": true,
	"郭": true, "梅": true, "盛": true, "林": true, "刁": true, "钟": true, "徐": true, "邱": true,
	"骆": true, "高": true, "夏": true, "蔡": true, "田": true, "樊": true, "胡": true, "凌": true,
	"霍": true, "虞": true, "万": true, "支": true, "柯": true, "管": true, "卢": true, "莫": true,
	"房": true, "解": true, "应": true, "丁": true, "宣": true, "邓": true, "郁": true, "单": true,
	"杭": true, "洪": true, "包": true, "诸": true, "左": true, "石": true, "崔": true, "吉": true,
	"钮": true, "龚": true, "程": true, "嵇": true, "邢": true, "滑": true, "裴": true, "陆": true,
	"荣": true, "翁": true, "荀": true, "羊": true, "甄": true, "封": true, "芮": true, "储": true,
	"靳": true, "汲": true, "邴": true, "糜": true, "松": true, "井": true, "段": true, "富": true,
	"巫": true, "乌": true, "焦": true, "巴": true, "弓": true, "牧": true, "隗": true, "山": true,
	"谷": true, "车": true, "侯": true, "宓": true, "蓬": true, "全": true, "郗": true, "班": true,
	"仰": true, "秋": true, "仲": true, "宫": true, "宁": true, "仇": true, "栾": true, "暴": true,
	"甘": true, "厉": true, "戎": true, "祖": true, "武": true, "符": true, "刘": true, "景": true,
	"詹": true, "束": true, "龙": true, "叶": true, "幸": true, "司": true, "韶": true, "黎": true,
	"蓟": true, "薄": true, "印": true, "宿": true, "白": true, "怀": true, "蒲": true, "从": true,
	"鄂": true, "索": true, "咸": true, "籍": true, "赖": true, "卓": true, "蔺": true, "屠": true,
	"池": true, "乔": true, "曾": true, "牛": true, "关": true, "燕": true, "文": true, "辛": true,
	"欧阳": true, "司马": true, "上官": true, "诸葛": true, "东方": true, "独孤": true, "南宫": true,
	"万俟": true, "闻人": true, "夏侯": true, "赫连": true, "皇甫": true, "尉迟": true, "公孙": true,
	"慕容": true, "长孙": true, "宇文": true, "司徒": true, "轩辕": true, "令狐": true, "呼延": true,
}

// cardDispatch Luhn 通过后按 IIN 前缀分发到 zh_bank_card / credit_card。
// 单一数字块只输出一个 type，避免重叠区间的二选一竞态（filterAndSort 行为）。
// 见 pkg/global.IsInternationalCard。
func cardDispatch(v string) (entityType string, ok bool) {
	if !global.ValidCreditCard(v) {
		return "", false
	}
	if global.IsInternationalCard(v) {
		return types.EntityCreditCard, true
	}
	return types.EntityBankCard, true
}

// NewRegexEngine 构造内置正则检测引擎。
func NewRegexEngine(opts ...Option) *RegexEngine {
	e := &RegexEngine{opts: newOptions(opts...)}
	e.rules = []rule{
		{types.EntityIDCard, 0.95, reIDCard18, boolToV(cn.ValidIDCard), false, true},
		{types.EntityIDCard, 0.85, reIDCard15, boolToV(cn.ValidIDCard), true, true},
		{types.EntityPhone, 0.95, rePhone, boolToV(cn.ValidPhone), true, false},
		// 银行卡 / 信用卡：13-19 位统一由 cardDispatch 分发（避免 score-tie 双丢）。
		// entityType 字段作 fallback（IIN 不命中时回退到 zh_bank_card）。
		{types.EntityBankCard, 0.9, reCreditCard, cardDispatch, true, false},
		{types.EntityEmail, 0.9, reEmail, nil, false, false},
		{types.EntityIPAddress, 0.8, reIPv4, nil, true, false},
		{types.EntityDate, 0.7, reDate, nil, false, false},
		{types.EntityAddress, 0.7, reAddress, nil, false, false},
		{types.EntityAPIKey, 0.9, reAPIKey, nil, false, false},
		{types.EntityToken, 0.9, reJWT, nil, false, false},
		// 英文/国际化基线（2026-09-11 拍板）：
		// 中文车牌严格（省份前缀字符集），直接进规则表；type=plate。
		{types.EntityPlate, 0.85, rePlate, nil, false, false},
		// 英文车牌 rePlateEN/rePlateCA 是上下文正则（带捕获组），走 scanPlateEN 专用扫描。
		// URL：必须通过 url.Parse + scheme/host 校验，避免误判。
		{types.EntityURL, 0.9, reURL, boolToV(global.ValidURL), false, false},
		// US SSN：必须通过 SSA 校验（area 排除 + group/serial 非零）。
		{types.EntityUSSSN, 0.9, reUSSSN, boolToV(global.ValidUSSSN), true, false},
	}

	// 形态专用规则：只跑「归一化窗口」，不进原文那一遍。
	//
	// 为什么不让它们扫全文：这两条的 pattern 带可选分隔符（`(?:[ \-]?\d){9}` 这类
	// 重复里嵌可选项），正则引擎需要大量回溯 —— 实测在 340 字节负载上单条就要
	// 14.5µs，占整次 Detect 的 ~5%，而绝大多数文本根本没有国家码/点分号码。
	// 放到窗口上跑，开销就只跟真的像号码的片段长度成正比（同样文本上降到 ~1/17）。
	// 代价是「本来就没有任何窗口」的文本不再被这两条规则覆盖 —— 但那正是它们
	// 只可能命中的形态，不存在漏检。
	e.shapeRules = []rule{
		{types.EntityPhone, 0.9, rePhoneDotted, validDottedPhoneV, true, false},
		{types.EntityPhone, 0.9, rePhoneCC, validCountryPhoneV, true, false},
	}

	// windowRules = 通用规则里「会被书写形态影响」的那些 + 形态专用规则。
	for _, r := range e.rules {
		if shapeSensitiveTypes[r.entityType] {
			e.windowRules = append(e.windowRules, r)
		}
	}
	e.windowRules = append(e.windowRules, e.shapeRules...)
	return e
}

// boolToV 把旧的 bool 校验器适配成新 (type, bool) 签名。
func boolToV(old func(string) bool) func(string) (string, bool) {
	if old == nil {
		return nil
	}
	return func(v string) (string, bool) {
		return "", old(v)
	}
}

// stripPhoneSep 去掉手机号书写形态里的分隔符与全角变体
// （`+86-186-1234-5678` → `+8618612345678`）。
func stripPhoneSep(v string) string {
	return strings.Map(func(r rune) rune {
		switch foldWidth(r) {
		case '+', '-', '_', '.', ' ':
			return -1
		}
		return r
	}, v)
}

// validCountryPhoneV 国家码手机号：剥掉 `+` / `00` / `86` 后按 11 位手机号校验。
//
// 必须有这一步：校验器拿到的是**带国家码的原文形态**，直接丢给 cn.ValidPhone
// 一定是 false（长度就不是 11）。
func validCountryPhoneV(v string) (string, bool) {
	s := strings.TrimPrefix(stripPhoneSep(v), "0086")
	s = strings.TrimPrefix(s, "86")
	return "", cn.ValidPhone(s)
}

// validDottedPhoneV 点分手机号：去掉 `.` 后按 11 位手机号校验。
func validDottedPhoneV(v string) (string, bool) {
	return "", cn.ValidPhone(stripPhoneSep(v))
}

// shapeSensitiveTypes 是会被「书写形态」影响的实体类型 —— 数字型固定格式实体。
//
// 只有它们值得在归一化文本上再扫一遍：矩阵语料里 dashed / spaced / grouped /
// fullwidth / country 五种形态共 173 个 GT 全部落在这几类上，一个不落。
//
// 人名 / 地址 / 邮箱 / 车牌**不在其列**：它们的写法里没有「数字夹分隔符」这种形态
// 问题，归一化对它们零增益，却可能在归一化文本上多出误报。
// `date` 与 `us_ssn` 也刻意不在其列 —— 它们的规则**依赖**分隔符，在归一化文本上
// 必然失效（见 normalize.go 文件头第 1 条）。
var shapeSensitiveTypes = map[string]bool{
	types.EntityPhone:      true,
	types.EntityIDCard:     true,
	types.EntityBankCard:   true,
	types.EntityCreditCard: true,
}

// Name 引擎名称（契约 §2.2）。
func (e *RegexEngine) Name() string { return "regex" }

// Detect 对单段文本做检测（契约 §2.2）。
func (e *RegexEngine) Detect(ctx context.Context, req *types.DetectRequest) (*types.DetectResponse, error) {
	start := time.Now()
	entities := e.scan(req.Text)
	entities = filterAndSort(entities, e.opts)
	return &types.DetectResponse{
		Entities:  entities,
		LatencyMs: time.Since(start).Milliseconds(),
	}, nil
}

// DetectBatch 批量检测，顺序与输入一致（契约 §2.2）。
func (e *RegexEngine) DetectBatch(ctx context.Context, reqs []*types.DetectRequest) ([]*types.DetectResponse, error) {
	out := make([]*types.DetectResponse, 0, len(reqs))
	for _, r := range reqs {
		resp, err := e.Detect(ctx, r)
		if err != nil {
			return nil, err
		}
		out = append(out, resp)
	}
	return out, nil
}

// Health 内置引擎永远健康（无外部依赖）。
func (e *RegexEngine) Health(ctx context.Context) error { return nil }

// scan 跑所有规则并附加凭证 KV / 人名启发式结果。
func (e *RegexEngine) scan(text string) []types.Entity {
	out := e.scanRules(text, e.rules)
	out = append(out, e.scanSecretKV(text)...)
	out = append(out, e.scanPersonName(text)...)
	out = append(out, e.scanPlateEN(text)...)

	// 形态容忍：把「含分隔符/全角的数字片段」归一化后再扫一遍「数字型固定格式」规则，
	// 结果映射回原文偏移。
	//
	// 这一步**只做加法**：归一化结果里凡是与已有区间相交的一律整条丢弃（overlapsAny），
	// 所以原文能检出的东西永远不会被归一化结果替换掉 —— 形态容忍不可能造成检测退化。
	// 它也**不替换原文**：原文那一遍原样保留，`date`/`us_ssn` 这类依赖分隔符的规则
	// 因此完全不受影响（normalize.go 文件头第 1 条）。
	//
	// 只在**窗口**上做归一化，不整篇做：理由见 normalize.go windows 的注释（性能）。
	for _, w := range windows(text) {
		norm, starts, ends := normalize(w.text)
		var nm *normMap
		if starts != nil {
			nm = &normMap{starts: starts, ends: ends}
		}
		// nm 为 nil 表示窗口「看起来是号码形态但无需归一化」（例如点分号码、或
		// `+86` 开头本来就干净的写法）—— 此时按恒等映射扫窗口原文即可。
		for _, ent := range e.scanRules(norm, e.windowRules) {
			ns, ne, ok := nm.toOrig(ent.Start, ent.End)
			if !ok {
				continue
			}
			os, oe := w.start+ns, w.start+ne
			if os >= oe || oe > len(text) || overlapsAny(out, os, oe) {
				continue
			}
			if !uniformSeparators(text, os, oe) {
				// 分隔符混用 ⇒ 很可能是把不相干的相邻 token 拼成了长数字串（例如
				// 「日期 + 两个四位数」），不是同一个人的分组写法。丢弃。
				continue
			}
			orig := text[os:oe]
			// 区间一致性自检：把映射回来的原文切片重新归一化，必须与归一化空间的
			// 匹配值逐字节相同。不符说明区间映射错位 —— 丢弃。宁可漏检，也绝不允许
			// 偏移错位导致脱敏改错地方。
			if back, _, _ := normalize(orig); back != ent.Value {
				continue
			}
			ent.Start, ent.End, ent.Value = os, oe, orig
			out = append(out, ent)
		}
	}
	return out
}

// scanRules 按给定规则集扫描文本。
//
// 原文与归一化文本共用它，保证两条路径的阈值、边界与校验判定不会各写一份。
func (e *RegexEngine) scanRules(text string, rules []rule) []types.Entity {
	var out []types.Entity
	for _, r := range rules {
		for _, m := range r.re.FindAllStringIndex(text, -1) {
			start, end := m[0], m[1]
			if r.digitBoundary && (hasDigitNeighbor(text, start, end)) {
				continue
			}
			if r.idBoundary && hasIDNeighbor(text, start, end) {
				continue
			}
			value := text[start:end]
			entType := r.entityType
			if r.validate != nil {
				t, ok := r.validate(value)
				if !ok {
					continue
				}
				if t != "" {
					entType = t
				}
			}
			out = append(out, types.Entity{
				Type: entType, Value: value, Start: start, End: end, Score: r.score,
			})
		}
	}
	return out
}

// overlapsAny 判断 [start, end) 是否与已有实体区间相交。
//
// 取「任何类型」而不是「同类型」：相交的两段区间在下游（filterAndSort 按分数、
// sanitizeEntities 按长度）只会留下一个，留下的那个更短就会残留未脱敏的明文。
// 归一化结果宁可整条丢弃，也不参与这种二选一。
func overlapsAny(ents []types.Entity, start, end int) bool {
	for _, e := range ents {
		if e.Start < end && start < e.End {
			return true
		}
	}
	return false
}

// span 是「同一段文本内已上报区间」的去重键。
//
// 为什么必须按区间而不是按值去重：一段文本里同一个 PII 值可以合法地出现多次
// （「我叫李杰琪，请转告李杰琪」）。按键值去重会只上报第一次，后续出现的明文就
// 直接泄漏到上游 —— 早期 scanPersonName / scanPlateEN 正是这么写的，实测可复现泄漏。
//
// 本层去重只解决「同一条规则或两条规则对**同一段**重复上报」；规则之间的区间重叠
// 由下游 replacer.sanitizeEntities 统一消解（保留更长者）。
type span struct{ start, end int }

// scanPlateEN 英文车牌：上下文引导（license/vehicle/registration plate）
// + 只报捕获组的车牌本体（不含引导词）。与 scanPersonName 同构。
// 弱格式实体在正则引擎下只保证精确率——无上下文的 "ABC-1234" 不报。
func (e *RegexEngine) scanPlateEN(text string) []types.Entity {
	var out []types.Entity
	seen := map[span]bool{}
	appendPlate := func(start, end int) {
		if seen[span{start, end}] {
			return
		}
		seen[span{start, end}] = true
		out = append(out, types.Entity{
			Type: types.EntityPlate, Value: text[start:end], Start: start, End: end, Score: 0.75,
		})
	}
	for _, m := range rePlateEN.FindAllStringSubmatchIndex(text, -1) {
		if len(m) >= 4 && m[2] >= 0 {
			appendPlate(m[2], m[3])
		}
	}
	for _, m := range rePlateCA.FindAllStringSubmatchIndex(text, -1) {
		if len(m) >= 4 && m[2] >= 0 {
			appendPlate(m[2], m[3])
		}
	}
	return out
}

// hasDigitNeighbor 左右邻字符是否为数字。
func hasDigitNeighbor(text string, start, end int) bool {
	if start > 0 {
		c := text[start-1]
		if c >= '0' && c <= '9' {
			return true
		}
	}
	if end < len(text) {
		c := text[end]
		if c >= '0' && c <= '9' {
			return true
		}
	}
	return false
}

// hasIDNeighbor 左右邻是否为数字或 X/x（身份证边界）。
func hasIDNeighbor(text string, start, end int) bool {
	check := func(c byte) bool {
		return (c >= '0' && c <= '9') || c == 'X' || c == 'x'
	}
	if start > 0 && check(text[start-1]) {
		return true
	}
	if end < len(text) && check(text[end]) {
		return true
	}
	return false
}

// scanSecretKV 对 key=value 形式的凭证，只脱敏值部分。
func (e *RegexEngine) scanSecretKV(text string) []types.Entity {
	var out []types.Entity
	for _, m := range reSecretKV.FindAllStringSubmatchIndex(text, -1) {
		if len(m) < 4 || m[2] < 0 {
			continue
		}
		key := strings.ToLower(text[m[0]:m[1]])
		entType := types.EntityPassword
		switch {
		case strings.Contains(key, "api_key") || strings.Contains(key, "apikey") || strings.Contains(key, "access_key"):
			entType = types.EntityAPIKey
		case strings.Contains(key, "token"):
			entType = types.EntityToken
		}
		out = append(out, types.Entity{
			Type: entType, Value: text[m[2]:m[3]], Start: m[2], End: m[3], Score: 0.9,
		})
	}
	return out
}

// nameParticles 绝不会出现在中文姓名里的结构助词 / 虚词。
//
// 用途见 trimNameParticle。刻意**不含**「和 / 有 / 会 / 能 / 要」：这几个字虽然也
// 常作虚词，却同样可以是真实姓名的末字（李永和、张有、陈会…）。把它们当截断信号，
// 会把「联系人李永和确认收到」截成「李永」—— 拿一个精确率问题换一个召回率问题，
// 不划算。它们造成的残留过捕获（「联系人张三和客户王五」）见 trimNameParticle 注释。
var nameParticles = map[rune]bool{
	'的': true, '了': true, '是': true, '在': true, '把': true, '被': true,
	'让': true, '不': true, '这': true, '那': true, '个': true, '些': true,
	'们': true, '吗': true, '呢': true, '吧': true, '就': true, '都': true,
	'也': true, '还': true, '很': true, '没': true, '给': true, '对': true,
	'从': true, '向': true, '该': true, '其': true, '并': true, '则': true,
	'等': true, '或': true, '而': true, '由': true, '及': true, '与': true,
	'为': true, '以': true, '之': true,
}

// trimNameParticle 把贪婪捕获的人名截回真实长度。
//
// 背景：`[一-龥]{2,4}` 是左对齐贪婪匹配，多吞的字总是紧跟在真实人名之后
// （「员工张三的身份证…」捕到 4 个 rune「张三的身」）。因此只需判断「多吞了几个」。
//
// 判定依据是中文姓名的构词约束：
//   - **4 rune**：四字名几乎只能是「复姓 + 双字名」（欧阳 / 司马 / 上官 / 诸葛 /
//     尉迟 / 令狐…）。前缀不是复姓 → 一定多吞了字：第 3 字是硬助词就只剩两字名
//     （张三的身 → 张三），否则是「三字名 + 多吞 1 字」（张伟明今 → 张伟明）。
//   - **3 rune**：第 3 字是硬助词 → 两字名 + 助词（张三的 → 张三）；否则原样保留
//     （李永和）。
//
// 已知残留：两字名后接「和 / 有 / 会 / 能 / 要」时会多留一个字
// （「联系人张三和客户王五」→ 张三和）。这些字是真实姓名的合法末字，正则层面
// 无法区分，交由 detection.engine=pii-engineer 的 NER 模型消解 —— 这与本引擎
// 「弱格式实体只保证精确率、完整召回依赖 NER」的定位一致。
func trimNameParticle(runes []rune) []rune {
	switch len(runes) {
	case 4:
		if commonSurnames[string(runes[:2])] {
			return runes // 合法的复姓四字名，不动
		}
		if nameParticles[runes[2]] {
			return runes[:2]
		}
		return runes[:3]
	case 3:
		if nameParticles[runes[2]] {
			return runes[:2]
		}
		return runes
	default:
		return runes
	}
}

// scanPersonName 高精度人名启发式：必须有强上下文或称谓，且首字在常见姓氏表内，
// 且贪婪捕获的尾部助词会被截掉（见 trimNameParticle）。
func (e *RegexEngine) scanPersonName(text string) []types.Entity {
	seen := map[span]bool{}
	var out []types.Entity
	appendName := func(start, end int, score float64) {
		runes := trimNameParticle([]rune(text[start:end]))
		// 截断后按 rune 数回算结束偏移（被截掉的部分不计入实体）。
		v := string(runes)
		end = start + len(v)
		if seen[span{start, end}] || len(runes) < 2 || len(runes) > 4 {
			return
		}
		if !commonSurnames[string(runes[0])] && !commonSurnames[string(runes[:2])] {
			return
		}
		seen[span{start, end}] = true
		out = append(out, types.Entity{
			Type: types.EntityPersonName, Value: v, Start: start, End: end, Score: score,
		})
	}
	for _, m := range reNameCtx.FindAllStringSubmatchIndex(text, -1) {
		if len(m) >= 4 && m[2] >= 0 {
			appendName(m[2], m[3], 0.75)
		}
	}
	for _, m := range reNameTile.FindAllStringSubmatchIndex(text, -1) {
		if len(m) >= 4 && m[2] >= 0 {
			appendName(m[2], m[3], 0.7)
		}
	}
	return out
}
