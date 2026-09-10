package detector

import (
	"context"
	"regexp"
	"strings"
	"time"

	"gateway/pkg/cn"
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
}

type rule struct {
	entityType string
	score      float64
	re         *regexp.Regexp
	validate   func(string) bool
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
	reBankCard = regexp.MustCompile(`[0-9]{16,19}`)
	reEmail    = regexp.MustCompile(`[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}`)
	reIPv4     = regexp.MustCompile(`(?:(?:25[0-5]|2[0-4][0-9]|1[0-9]{2}|[1-9]?[0-9])\.){3}(?:25[0-5]|2[0-4][0-9]|1[0-9]{2}|[1-9]?[0-9])`)
	reDate     = regexp.MustCompile(`(?:19|20)[0-9]{2}[-/.年](?:0?[1-9]|1[0-2])[-/.月](?:0?[1-9]|[12][0-9]|3[01])日?`)
	// ⚠️ 冲突标注（2026-09-10，待裁决，见 SPEC_ALIGNMENT.md C12d / Q12）：
	// rePlate 已定义但**未接线**——pkg/types 没有对应实体类型常量，车牌不会出现在检测输出中。
	// 而 README「支持的实体类型」表把"车牌"列为已支持，与实现不符。
	// 方案利弊：① 接线（新增 zh_plate 类型）——扩检测面，但需补 ground truth 语料并重跑 bench；
	//          ② 维持未接线 + 从 README 移除——文档诚实，但对外承诺缩小。
	// 当前未改动检测行为（新增实体类型属功能变更，需人工裁决）。
	rePlate    = regexp.MustCompile(`[京津沪渝冀豫云辽黑湘皖鲁新苏浙赣鄂桂甘晋蒙陕吉闽贵粤青藏川宁琼][A-Z][A-HJ-NP-Z0-9]{4,6}`)
	reAPIKey   = regexp.MustCompile(`\b(?:sk|pk|api|ak)-[A-Za-z0-9_\-]{16,}`)
	reJWT      = regexp.MustCompile(`\beyJ[A-Za-z0-9_\-]{8,}\.[A-Za-z0-9_\-]{8,}\.[A-Za-z0-9_\-]{8,}`)
	reSecretKV = regexp.MustCompile(`(?i)\b(?:api[_\-]?key|secret|password|passwd|pwd|token|access[_\-]?key)\b\s*[:=]\s*["']?([A-Za-z0-9_\-\.]{8,})["']?`)
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

// NewRegexEngine 构造内置正则检测引擎。
func NewRegexEngine(opts ...Option) *RegexEngine {
	e := &RegexEngine{opts: newOptions(opts...)}
	e.rules = []rule{
		{types.EntityIDCard, 0.95, reIDCard18, cn.ValidIDCard, false, true},
		{types.EntityIDCard, 0.85, reIDCard15, cn.ValidIDCard, true, true},
		{types.EntityPhone, 0.95, rePhone, cn.ValidPhone, true, false},
		{types.EntityBankCard, 0.9, reBankCard, cn.ValidBankCard, true, false},
		{types.EntityEmail, 0.9, reEmail, nil, false, false},
		{types.EntityIPAddress, 0.8, reIPv4, nil, true, false},
		{types.EntityDate, 0.7, reDate, nil, false, false},
		{types.EntityAddress, 0.7, reAddress, nil, false, false},
		{types.EntityAPIKey, 0.9, reAPIKey, nil, false, false},
		{types.EntityToken, 0.9, reJWT, nil, false, false},
	}
	return e
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
	var out []types.Entity
	for _, r := range e.rules {
		for _, m := range r.re.FindAllStringIndex(text, -1) {
			start, end := m[0], m[1]
			if r.digitBoundary && (hasDigitNeighbor(text, start, end)) {
				continue
			}
			if r.idBoundary && hasIDNeighbor(text, start, end) {
				continue
			}
			value := text[start:end]
			if r.validate != nil && !r.validate(value) {
				continue
			}
			out = append(out, types.Entity{
				Type: r.entityType, Value: value, Start: start, End: end, Score: r.score,
			})
		}
	}
	out = append(out, e.scanSecretKV(text)...)
	out = append(out, e.scanPersonName(text)...)
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

// scanPersonName 高精度人名启发式：必须有强上下文或称谓，且首字在常见姓氏表内。
func (e *RegexEngine) scanPersonName(text string) []types.Entity {
	seen := map[string]bool{}
	var out []types.Entity
	appendName := func(start, end int, score float64) {
		v := text[start:end]
		runes := []rune(v)
		if seen[v] || len(runes) < 2 || len(runes) > 4 {
			return
		}
		if !commonSurnames[string(runes[0])] && !commonSurnames[string(runes[:2])] {
			return
		}
		seen[v] = true
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
