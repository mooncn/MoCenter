package geo

// 国家 / 地区名称与 ISO 3166-1 alpha-2 代码对照表。
//
// 为什么需要它：主源 api.iping.cc **只返回国家名称、不返回国家代码**，
// 而公开页的国旗由 country_code 生成 —— 没有这张表就会出现
// "地区显示正常但国旗空白"。表中同时收录英文名与简体中文名，
// 任意一个源返回的中英文国名都能反查出代码。
//
// 生成口径：ISO 3166-1（英文名取官方名的短形式）与 CLDR 的简体中文名。
// 只做**名称 -> 代码**的归一，不参与任何业务判断：
// 查不到时保留上游原值，宁可不显示国旗，也不要把国家写错。

// countryEntry 一行国家/地区对照：ISO 代码 + 英文名 + 简体中文名（可能为空）。
type countryEntry struct {
	Code string
	EN   string
	ZH   string
}

// countries 按 ISO 代码升序排列。
var countries = []countryEntry{
	{"AD", "Andorra", "安道尔"},
	{"AE", "United Arab Emirates", "阿拉伯联合酋长国"},
	{"AF", "Afghanistan", "阿富汗"},
	{"AG", "Antigua and Barbuda", "安提瓜和巴布达"},
	{"AI", "Anguilla", "安圭拉"},
	{"AL", "Albania", "阿尔巴尼亚"},
	{"AM", "Armenia", "亚美尼亚"},
	{"AO", "Angola", "安哥拉"},
	{"AQ", "Antarctica", "南极洲"},
	{"AR", "Argentina", "阿根廷"},
	{"AS", "American Samoa", "美属萨摩亚"},
	{"AT", "Austria", "奥地利"},
	{"AU", "Australia", "澳大利亚"},
	{"AW", "Aruba", "阿鲁巴"},
	{"AX", "Åland Islands", "奥兰群岛"},
	{"AZ", "Azerbaijan", "阿塞拜疆"},
	{"BA", "Bosnia and Herzegovina", "波斯尼亚和黑塞哥维那"},
	{"BB", "Barbados", "巴巴多斯"},
	{"BD", "Bangladesh", "孟加拉国"},
	{"BE", "Belgium", "比利时"},
	{"BF", "Burkina Faso", "布基纳法索"},
	{"BG", "Bulgaria", "保加利亚"},
	{"BH", "Bahrain", "巴林"},
	{"BI", "Burundi", "布隆迪"},
	{"BJ", "Benin", "贝宁"},
	{"BL", "Saint Barthélemy", "圣巴泰勒米"},
	{"BM", "Bermuda", "百慕大"},
	{"BN", "Brunei Darussalam", "文莱"},
	{"BO", "Bolivia", "玻利维亚"},
	{"BQ", "Bonaire", "荷属加勒比区"},
	{"BR", "Brazil", "巴西"},
	{"BS", "Bahamas", "巴哈马"},
	{"BT", "Bhutan", "不丹"},
	{"BV", "Bouvet Island", "布韦岛"},
	{"BW", "Botswana", "博茨瓦纳"},
	{"BY", "Belarus", "白俄罗斯"},
	{"BZ", "Belize", "伯利兹"},
	{"CA", "Canada", "加拿大"},
	{"CC", "Cocos (Keeling) Islands", "科科斯（基林）群岛"},
	{"CD", "Congo", "刚果（金）"},
	{"CF", "Central African Republic", "中非共和国"},
	{"CG", "Congo", "刚果（布）"},
	{"CH", "Switzerland", "瑞士"},
	{"CI", "Côte d'Ivoire", "科特迪瓦"},
	{"CK", "Cook Islands", "库克群岛"},
	{"CL", "Chile", "智利"},
	{"CM", "Cameroon", "喀麦隆"},
	{"CN", "China", "中国"},
	{"CO", "Colombia", "哥伦比亚"},
	{"CR", "Costa Rica", "哥斯达黎加"},
	{"CU", "Cuba", "古巴"},
	{"CV", "Cabo Verde", "佛得角"},
	{"CW", "Curaçao", "库拉索"},
	{"CX", "Christmas Island", "圣诞岛"},
	{"CY", "Cyprus", "塞浦路斯"},
	{"CZ", "Czechia", "捷克"},
	{"DE", "Germany", "德国"},
	{"DJ", "Djibouti", "吉布提"},
	{"DK", "Denmark", "丹麦"},
	{"DM", "Dominica", "多米尼克"},
	{"DO", "Dominican Republic", "多米尼加共和国"},
	{"DZ", "Algeria", "阿尔及利亚"},
	{"EC", "Ecuador", "厄瓜多尔"},
	{"EE", "Estonia", "爱沙尼亚"},
	{"EG", "Egypt", "埃及"},
	{"EH", "Western Sahara", "西撒哈拉"},
	{"ER", "Eritrea", "厄立特里亚"},
	{"ES", "Spain", "西班牙"},
	{"ET", "Ethiopia", "埃塞俄比亚"},
	{"FI", "Finland", "芬兰"},
	{"FJ", "Fiji", "斐济"},
	{"FK", "Falkland Islands (Malvinas)", "福克兰群岛"},
	{"FM", "Micronesia", "密克罗尼西亚"},
	{"FO", "Faroe Islands", "法罗群岛"},
	{"FR", "France", "法国"},
	{"GA", "Gabon", "加蓬"},
	{"GB", "United Kingdom of Great Britain and Northern Ireland", "英国"},
	{"GD", "Grenada", "格林纳达"},
	{"GE", "Georgia", "格鲁吉亚"},
	{"GF", "French Guiana", "法属圭亚那"},
	{"GG", "Guernsey", "根西岛"},
	{"GH", "Ghana", "加纳"},
	{"GI", "Gibraltar", "直布罗陀"},
	{"GL", "Greenland", "格陵兰"},
	{"GM", "Gambia", "冈比亚"},
	{"GN", "Guinea", "几内亚"},
	{"GP", "Guadeloupe", "瓜德罗普"},
	{"GQ", "Equatorial Guinea", "赤道几内亚"},
	{"GR", "Greece", "希腊"},
	{"GS", "South Georgia and the South Sandwich Islands", "南乔治亚和南桑威奇群岛"},
	{"GT", "Guatemala", "危地马拉"},
	{"GU", "Guam", "关岛"},
	{"GW", "Guinea-Bissau", "几内亚比绍"},
	{"GY", "Guyana", "圭亚那"},
	{"HK", "Hong Kong", "中国香港特别行政区"},
	{"HM", "Heard Island and McDonald Islands", "赫德岛和麦克唐纳群岛"},
	{"HN", "Honduras", "洪都拉斯"},
	{"HR", "Croatia", "克罗地亚"},
	{"HT", "Haiti", "海地"},
	{"HU", "Hungary", "匈牙利"},
	{"ID", "Indonesia", "印度尼西亚"},
	{"IE", "Ireland", "爱尔兰"},
	{"IL", "Israel", "以色列"},
	{"IM", "Isle of Man", "马恩岛"},
	{"IN", "India", "印度"},
	{"IO", "British Indian Ocean Territory", "英属印度洋领地"},
	{"IQ", "Iraq", "伊拉克"},
	{"IR", "Iran", "伊朗"},
	{"IS", "Iceland", "冰岛"},
	{"IT", "Italy", "意大利"},
	{"JE", "Jersey", "泽西岛"},
	{"JM", "Jamaica", "牙买加"},
	{"JO", "Jordan", "约旦"},
	{"JP", "Japan", "日本"},
	{"KE", "Kenya", "肯尼亚"},
	{"KG", "Kyrgyzstan", "吉尔吉斯斯坦"},
	{"KH", "Cambodia", "柬埔寨"},
	{"KI", "Kiribati", "基里巴斯"},
	{"KM", "Comoros", "科摩罗"},
	{"KN", "Saint Kitts and Nevis", "圣基茨和尼维斯"},
	{"KP", "Korea", "朝鲜"},
	{"KR", "Korea", "韩国"},
	{"KW", "Kuwait", "科威特"},
	{"KY", "Cayman Islands", "开曼群岛"},
	{"KZ", "Kazakhstan", "哈萨克斯坦"},
	{"LA", "Lao People's Democratic Republic", "老挝"},
	{"LB", "Lebanon", "黎巴嫩"},
	{"LC", "Saint Lucia", "圣卢西亚"},
	{"LI", "Liechtenstein", "列支敦士登"},
	{"LK", "Sri Lanka", "斯里兰卡"},
	{"LR", "Liberia", "利比里亚"},
	{"LS", "Lesotho", "莱索托"},
	{"LT", "Lithuania", "立陶宛"},
	{"LU", "Luxembourg", "卢森堡"},
	{"LV", "Latvia", "拉脱维亚"},
	{"LY", "Libya", "利比亚"},
	{"MA", "Morocco", "摩洛哥"},
	{"MC", "Monaco", "摩纳哥"},
	{"MD", "Moldova", "摩尔多瓦"},
	{"ME", "Montenegro", "黑山"},
	{"MF", "Saint Martin (French part)", "法属圣马丁"},
	{"MG", "Madagascar", "马达加斯加"},
	{"MH", "Marshall Islands", "马绍尔群岛"},
	{"MK", "North Macedonia", "北马其顿"},
	{"ML", "Mali", "马里"},
	{"MM", "Myanmar", "缅甸"},
	{"MN", "Mongolia", "蒙古"},
	{"MO", "Macao", "中国澳门特别行政区"},
	{"MP", "Northern Mariana Islands", "北马里亚纳群岛"},
	{"MQ", "Martinique", "马提尼克"},
	{"MR", "Mauritania", "毛里塔尼亚"},
	{"MS", "Montserrat", "蒙特塞拉特"},
	{"MT", "Malta", "马耳他"},
	{"MU", "Mauritius", "毛里求斯"},
	{"MV", "Maldives", "马尔代夫"},
	{"MW", "Malawi", "马拉维"},
	{"MX", "Mexico", "墨西哥"},
	{"MY", "Malaysia", "马来西亚"},
	{"MZ", "Mozambique", "莫桑比克"},
	{"NA", "Namibia", "纳米比亚"},
	{"NC", "New Caledonia", "新喀里多尼亚"},
	{"NE", "Niger", "尼日尔"},
	{"NF", "Norfolk Island", "诺福克岛"},
	{"NG", "Nigeria", "尼日利亚"},
	{"NI", "Nicaragua", "尼加拉瓜"},
	{"NL", "Netherlands", "荷兰"},
	{"NO", "Norway", "挪威"},
	{"NP", "Nepal", "尼泊尔"},
	{"NR", "Nauru", "瑙鲁"},
	{"NU", "Niue", "纽埃"},
	{"NZ", "New Zealand", "新西兰"},
	{"OM", "Oman", "阿曼"},
	{"PA", "Panama", "巴拿马"},
	{"PE", "Peru", "秘鲁"},
	{"PF", "French Polynesia", "法属波利尼西亚"},
	{"PG", "Papua New Guinea", "巴布亚新几内亚"},
	{"PH", "Philippines", "菲律宾"},
	{"PK", "Pakistan", "巴基斯坦"},
	{"PL", "Poland", "波兰"},
	{"PM", "Saint Pierre and Miquelon", "圣皮埃尔和密克隆群岛"},
	{"PN", "Pitcairn", "皮特凯恩群岛"},
	{"PR", "Puerto Rico", "波多黎各"},
	{"PS", "Palestine", "巴勒斯坦领土"},
	{"PT", "Portugal", "葡萄牙"},
	{"PW", "Palau", "帕劳"},
	{"PY", "Paraguay", "巴拉圭"},
	{"QA", "Qatar", "卡塔尔"},
	{"RE", "Réunion", "留尼汪"},
	{"RO", "Romania", "罗马尼亚"},
	{"RS", "Serbia", "塞尔维亚"},
	{"RU", "Russian Federation", "俄罗斯"},
	{"RW", "Rwanda", "卢旺达"},
	{"SA", "Saudi Arabia", "沙特阿拉伯"},
	{"SB", "Solomon Islands", "所罗门群岛"},
	{"SC", "Seychelles", "塞舌尔"},
	{"SD", "Sudan", "苏丹"},
	{"SE", "Sweden", "瑞典"},
	{"SG", "Singapore", "新加坡"},
	{"SH", "Saint Helena", "圣赫勒拿"},
	{"SI", "Slovenia", "斯洛文尼亚"},
	{"SJ", "Svalbard and Jan Mayen", "斯瓦尔巴和扬马延"},
	{"SK", "Slovakia", "斯洛伐克"},
	{"SL", "Sierra Leone", "塞拉利昂"},
	{"SM", "San Marino", "圣马力诺"},
	{"SN", "Senegal", "塞内加尔"},
	{"SO", "Somalia", "索马里"},
	{"SR", "Suriname", "苏里南"},
	{"SS", "South Sudan", "南苏丹"},
	{"ST", "Sao Tome and Principe", "圣多美和普林西比"},
	{"SV", "El Salvador", "萨尔瓦多"},
	{"SX", "Sint Maarten (Dutch part)", "荷属圣马丁"},
	{"SY", "Syrian Arab Republic", "叙利亚"},
	{"SZ", "Eswatini", "斯威士兰"},
	{"TC", "Turks and Caicos Islands", "特克斯和凯科斯群岛"},
	{"TD", "Chad", "乍得"},
	{"TF", "French Southern Territories", "法属南部领地"},
	{"TG", "Togo", "多哥"},
	{"TH", "Thailand", "泰国"},
	{"TJ", "Tajikistan", "塔吉克斯坦"},
	{"TK", "Tokelau", "托克劳"},
	{"TL", "Timor-Leste", "东帝汶"},
	{"TM", "Turkmenistan", "土库曼斯坦"},
	{"TN", "Tunisia", "突尼斯"},
	{"TO", "Tonga", "汤加"},
	{"TR", "Türkiye", "土耳其"},
	{"TT", "Trinidad and Tobago", "特立尼达和多巴哥"},
	{"TV", "Tuvalu", "图瓦卢"},
	{"TW", "Taiwan", "台湾"},
	{"TZ", "Tanzania", "坦桑尼亚"},
	{"UA", "Ukraine", "乌克兰"},
	{"UG", "Uganda", "乌干达"},
	{"UM", "United States Minor Outlying Islands", "美国本土外小岛屿"},
	{"US", "United States", "美国"},
	{"UY", "Uruguay", "乌拉圭"},
	{"UZ", "Uzbekistan", "乌兹别克斯坦"},
	{"VA", "Holy See", "梵蒂冈"},
	{"VC", "Saint Vincent and the Grenadines", "圣文森特和格林纳丁斯"},
	{"VE", "Venezuela", "委内瑞拉"},
	{"VG", "Virgin Islands (British)", "英属维尔京群岛"},
	{"VI", "Virgin Islands (U.S.)", "美属维尔京群岛"},
	{"VN", "Viet Nam", "越南"},
	{"VU", "Vanuatu", "瓦努阿图"},
	{"WF", "Wallis and Futuna", "瓦利斯和富图纳"},
	{"WS", "Samoa", "萨摩亚"},
	{"YE", "Yemen", "也门"},
	{"YT", "Mayotte", "马约特"},
	{"ZA", "South Africa", "南非"},
	{"ZM", "Zambia", "赞比亚"},
	{"ZW", "Zimbabwe", "津巴布韦"},
}

// countryIndex 名称 -> 对照行的索引（英文名与中文名都建索引，统一小写去空格）。
var countryIndex = func() map[string]countryEntry {
	m := make(map[string]countryEntry, len(countries)*2)
	for _, c := range countries {
		if k := countryKey(c.EN); k != "" {
			m[k] = c
		}
		if k := countryKey(c.ZH); k != "" {
			if _, ok := m[k]; !ok {
				m[k] = c
			}
		}
	}
	// 常见别名/写法差异：上游数据源的国家名并不总是严格等于 ISO 官方名
	aliases := map[string]string{
		"united states of america": "US",
		"united states":            "US",
		"usa":                      "US",
		"russian federation":       "RU",
		"korea republic of":        "KR",
		"republic of korea":        "KR",
		"south korea":              "KR",
		"korea":                    "KR",
		"viet nam":                 "VN",
		"czech republic":           "CZ",
		"hong kong sar":            "HK",
		"hong kong sar china":      "HK",
		"macau":                    "MO",
		"taiwan province of china": "TW",
		"iran islamic republic of": "IR",
		"bolivia":                  "BO",
		"venezuela":                "VE",
		"tanzania":                 "TZ",
		"moldova":                  "MD",
		"syria":                    "SY",
		"laos":                     "LA",
		"brunei":                   "BN",
		"vatican city":             "VA",
		"palestine":                "PS",
		"中国香港":                     "HK",
		"中国澳门":                     "MO",
		"香港":                       "HK",
		"澳门":                       "MO",
		"台湾省":                      "TW",
		"中国大陆":                     "CN",
	}
	for name, code := range aliases {
		e, ok := byCode(code)
		if !ok {
			continue
		}
		if k := countryKey(name); k != "" {
			m[k] = e
		}
	}
	return m
}()

// byCode 按 ISO 代码查对照行。
func byCode(code string) (countryEntry, bool) {
	for _, c := range countries {
		if c.Code == code {
			return c, true
		}
	}
	return countryEntry{}, false
}

// lookupCountry 按国家名（英文或中文）反查 ISO 代码与统一英文名。
// 返回 ok=false 表示表中没有这个名称，调用方应保留原值。
func lookupCountry(name string) (code, en string, ok bool) {
	e, ok := countryIndex[countryKey(name)]
	if !ok {
		return "", "", false
	}
	return e.Code, e.EN, true
}

// countryKey 归一化用于查表的名称：
//   - 字母统一小写；
//   - 连续空白折叠为单个空格，首尾空白去掉；
//   - 逗号/句点等标点去掉（"Korea, Republic of" 与 "Korea Republic of" 同类）；
//   - 结尾句号同理被去掉（"U.S.A." 之类的非规范写法）。
//
// 举例："South Korea" -> "south korea"，"Taiwan, Province of China" ->
// "taiwan province of china"。
func countryKey(s string) string {
	out := make([]byte, 0, len(s))
	pendingSpace := false
	for i := 0; i < len(s); i++ {
		b := s[i]
		switch {
		case b == ' ' || b == '\t' || b == '\n' || b == '\r':
			pendingSpace = len(out) > 0
		case b >= 'A' && b <= 'Z':
			if pendingSpace {
				out = append(out, ' ')
				pendingSpace = false
			}
			out = append(out, b+('a'-'A'))
		case b == ',' || b == '.' || b == '\'' || b == '(' || b == ')':
			// 标点不参与匹配，折叠后与无标点写法等价
			continue
		default:
			if pendingSpace {
				out = append(out, ' ')
				pendingSpace = false
			}
			out = append(out, b)
		}
	}
	return string(out)
}
