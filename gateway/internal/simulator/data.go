package simulator

import "gateway/pkg/types"

// 中文仿真替换所需的基础词库。
// 说明：全部为通用常见汉字/公开信息，不含任何真实自然人信息。

// singleSurnameList 常见单姓。
var singleSurnameList = []string{
	"王", "李", "张", "刘", "陈", "杨", "黄", "赵", "周", "吴",
	"徐", "孙", "马", "朱", "胡", "郭", "何", "高", "林", "罗",
	"郑", "梁", "谢", "宋", "唐", "许", "邓", "冯", "韩", "曹",
	"曾", "彭", "萧", "蔡", "潘", "田", "董", "袁", "于", "余",
	"叶", "蒋", "杜", "苏", "魏", "程", "吕", "丁", "沈", "任",
	"姚", "卢", "傅", "钟", "姜", "崔", "谭", "廖", "范", "汪",
	"陆", "金", "石", "戴", "贾", "韦", "夏", "邱", "方", "侯",
	"邹", "熊", "孟", "秦", "白", "江", "阎", "薛", "尹", "段",
	"雷", "黎", "史", "龙", "陶", "贺", "顾", "毛", "郝", "龚",
	"邵", "万", "钱", "严", "覃", "武", "戚", "莫", "孔", "向",
}

// compoundSurnameList 常见复姓。
var compoundSurnameList = []string{
	"欧阳", "司马", "上官", "诸葛", "东方", "独孤", "南宫", "万俟",
	"闻人", "夏侯", "赫连", "皇甫", "尉迟", "公孙", "慕容", "长孙",
	"宇文", "司徒", "轩辕", "令狐", "呼延", "端木", "太史", "第五",
}

// compoundSurnames 复姓查表。
var compoundSurnames = func() map[string]bool {
	m := make(map[string]bool, len(compoundSurnameList))
	for _, s := range compoundSurnameList {
		m[s] = true
	}
	return m
}()

// givenChars 人名常用字（中性，避免明显性别指向）。
var givenChars = []rune(
	"安柏承誠楚川慈丹德恩帆芳菲枫岗格冠冠海涵翰昊和恒华慧嘉建 Jiang" +
		"江杰捷金锦 Jing Jing Jing 静君俊凯康可朗乐雷莉莲亮琳伶" +
		"菱凌流柳隆鲁陆露鹭麓伦罗满茂梅美萌梦苗妙民敏名铭墨牧" +
		"慕念宁牛农努暖朋平朴其奇启齐奇谦强乔巧琴青清晴庆琼秋" +
		"然仁忍荣融柔如瑞若睿三山善上少邵绍升生诗石时实矢世势" +
		"是书叔舒树双水顺舜硕思松苏素泰谈桃陶天田甜听庭同彤桐" +
		"童图婉万望威为维伟卫蔚文闻问吾武希熙曦霞夏仙先贤显宪" +
		"献相香湘祥想向湘潇小晓孝新心昕欣鑫信星行幸雄修秀绣旭" +
		"轩宣学雪循雅亚严言妍研岩延炎衍燕扬阳杨洋耀业叶一伊衣" +
		"依仪宜义艺忆亦异益逸翼银英迎影勇友有宇雨玉育元园原源" +
		"远月悦跃云韵泽哲珍真臻正之类直芷志中钟舟周朱竹筑专庄" +
		"壮追卓子紫自宗祖")

// bankBINs 公开银行标识代码（BIN），仅取发卡行标识段，不含真实账号。
var bankBINs = []string{
	"622202", // 工商银行
	"622848", // 农业银行
	"621700", // 建设银行
	"622262", // 交通银行
	"621288", // 中国银行
	"622588", // 招商银行
	"622521", // 浦发银行
	"622609", // 中信银行
	"622998", // 民生银行
	"623058", // 平安银行
}

// emailWords 邮箱本地部分用词。
var emailWords = []string{
	"li", "wang", "zhang", "liu", "chen", "yang", "huang", "zhao", "wu", "zhou",
	"xu", "sun", "ma", "zhu", "hu", "guo", "lin", "luo", "zheng", "liang",
	"alex", "bob", "carol", "david", "eric", "frank", "grace", "henry", "ivan", "julia",
	"dev", "admin", "ops", "test", "info", "contact", "service", "support",
}

// provinceCities 省级 → 常见城市（用于地址仿真）。
var provinceCities = map[string][]string{
	"北京市": {"北京市"}, "上海市": {"上海市"}, "天津市": {"天津市"}, "重庆市": {"重庆市"},
	"广东省": {"广州市", "深圳市", "珠海市", "东莞市", "佛山市"},
	"浙江省": {"杭州市", "宁波市", "温州市", "嘉兴市", "绍兴市"},
	"江苏省": {"南京市", "苏州市", "无锡市", "常州市", "徐州市"},
	"山东省": {"济南市", "青岛市", "烟台市", "潍坊市"},
	"四川省": {"成都市", "绵阳市", "德阳市", "宜宾市"},
	"湖北省": {"武汉市", "宜昌市", "襄阳市"},
	"湖南省": {"长沙市", "株洲市", "湘潭市"},
	"河南省": {"郑州市", "洛阳市", "南阳市"},
	"福建省": {"福州市", "厦门市", "泉州市"},
	"安徽省": {"合肥市", "芜湖市", "蚌埠市"},
	"陕西省": {"西安市", "宝鸡市", "咸阳市"},
	"辽宁省": {"沈阳市", "大连市", "鞍山市"},
	"云南省": {"昆明市", "大理市", "丽江市"},
}

// districtNames 常见区名。
var districtNames = []string{
	"海淀区", "朝阳区", "西城区", "东城区", "浦东新区", "徐汇区", "静安区",
	"天河区", "越秀区", "南山区", "福田区", "西湖区", "滨江区", "鼓楼区",
	"玄武区", "武侯区", "锦江区", "洪山区", "武昌区", "雨花区", "天心区",
}

// streetNames 常见路名。
var streetNames = []string{
	"中关村大街", "世纪大道", "南京路", "解放大道", "人民路", "中山路",
	"建设路", "文化路", "科技大道", "创新路", "新华大街", "长江大道",
	"黄河路", "珠江路", "天府大道", "高新大道", "滨海大道", "环城东路",
}

// fakeAddress 逆向生成中文地址：省 → 市 → 区 → 街道 → 门牌（契约 §6.1）。
func (g *Generator) fakeAddress(v string, key []byte) string {
	r := rngFor(key, types.EntityAddress, []byte(v))

	provinces := make([]string, 0, len(provinceCities))
	for p := range provinceCities {
		provinces = append(provinces, p)
	}
	// map 遍历无序会导致同一输入不同输出，显式排序保证双射
	sortStrings(provinces)

	province := provinces[r.Intn(len(provinces))]
	cities := provinceCities[province]
	city := cities[r.Intn(len(cities))]
	district := districtNames[r.Intn(len(districtNames))]
	street := streetNames[r.Intn(len(streetNames))]
	number := 1 + r.Intn(999)
	return province + city + district + street + itoa(number) + "号"
}

// sortStrings 升序排序，保证仿真生成的确定性（map 遍历无序）。
func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}
