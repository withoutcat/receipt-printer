package main

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"text/template"
	"unsafe"

	escpos "github.com/DevLumuz/go-escpos"
	"golang.org/x/text/encoding/simplifiedchinese"
	"golang.org/x/text/transform"
	"gopkg.in/yaml.v3"
)

type Config struct {
	Printer PrinterConfig `yaml:"printer"`
	Receipt Receipt       `yaml:"receipt"`
}

type PrinterConfig struct {
	Columns  int    `yaml:"columns"`
	Encoding string `yaml:"encoding"`
}

var vtEnabled bool

var errUserCancel = errors.New("用户取消")

type Receipt struct {
	StoreName   string `yaml:"store_name"`
	StartTime   string `yaml:"start_time"`
	EndTime     string `yaml:"end_time"`
	MarketType  string `yaml:"market_type"`
	Area        string `yaml:"area"`
	Status      string `yaml:"status"`
	PrintPerson string `yaml:"print_person"`
	PrintTime   string `yaml:"print_time"`

	Turnover           float64 // 程序计算：纯收金额之和
	ItemConsumption    float64 // 程序计算：= 营业额
	ServiceFee         float64 `yaml:"service_fee"`
	MinConsumptionFill float64 `yaml:"min_consumption_fill"`
	DiscountTotal      float64 `yaml:"discount_total"`
	MemberDiscount     float64 `yaml:"member_discount"`
	PromotionDiscount  float64 `yaml:"promotion_discount"`
	GiftDiscount       float64 `yaml:"gift_discount"`
	DiscountAmount     float64 `yaml:"discount_amount"`
	FixedDiscount      float64 `yaml:"fixed_discount"`
	Rounding           float64 `yaml:"rounding"`
	Revenue           float64 // 程序计算：营业额 - 优惠金额

	WechatPay      float64 `yaml:"wechat_pay"`
	AlipaySubsidy  float64 `yaml:"alipay_subsidy"`
	Cash           float64 `yaml:"cash"`
	Alipay         float64 `yaml:"alipay"`
	MeituanGroup   float64 `yaml:"meituan_group"`
	MeituanWaimai  float64 `yaml:"meituan_waimai"`
	BillCount      float64 `yaml:"bill_count"`
	OpenTableCount float64 `yaml:"open_table_count"`
	GuestFlow      float64 `yaml:"guest_flow"`
	AvgBill        float64 // 程序计算：营业额 ÷ 账单数
	AvgTable       float64 // 程序计算：营业额 + 开台数
	AvgPerson      float64 // 程序计算：营业额 ÷ 客流量
	AvgDiningTime  float64 `yaml:"avg_dining_time"`
}

// main 是程序入口：
// 加载配置/模板（一次）→ 同页面交互选择打印机 → 打印 → 显示结果并提供 [Enter]再次打印 / [C]更改打印机 / [Esc]退出。
func main() {
	baseDir, err := exeDir()
	if err != nil {
		fail("无法获取程序目录", err)
		return
	}

	restoreOut, vtOK := enableVTOutput()
	vtEnabled = vtOK
	if restoreOut != nil {
		defer restoreOut()
	}

	bannerText, _ := readBanner(filepath.Join(baseDir, "banner"))

	logStep("🧾", "读取配置...")
	cfg, err := readConfig(filepath.Join(baseDir, "config.yaml"))
	if err != nil {
		fail("读取 config.yaml 失败", err)
		pause("按 Enter 退出")
		return
	}
	if cfg.Printer.Columns <= 0 {
		cfg.Printer.Columns = 48
	}
	if strings.TrimSpace(cfg.Printer.Encoding) == "" {
		cfg.Printer.Encoding = "utf-8"
	}

	logStep("🧩", "读取模板...")
	templateText, err := os.ReadFile(filepath.Join(baseDir, "template"))
	if err != nil {
		fail("读取 template 失败", err)
		pause("按 Enter 退出")
		return
	}

	logStep("🧠", "计算指标...")
	// 营业额 = 纯收金额各项之和
	cfg.Receipt.Turnover = cfg.Receipt.Alipay + cfg.Receipt.WechatPay + cfg.Receipt.Cash + cfg.Receipt.AlipaySubsidy + cfg.Receipt.MeituanGroup
	// 品项消费 = 营业额
	cfg.Receipt.ItemConsumption = cfg.Receipt.Turnover
	// 营业收入 = 营业额 - 优惠金额
	cfg.Receipt.Revenue = cfg.Receipt.Turnover - cfg.Receipt.DiscountTotal
	// 单均消费 = 营业额 ÷ 账单数
	if cfg.Receipt.BillCount > 0 {
		cfg.Receipt.AvgBill = cfg.Receipt.Turnover / cfg.Receipt.BillCount
	}
	// 桌均消费 = 营业额 ÷ 开台数
	if cfg.Receipt.OpenTableCount > 0 {
		cfg.Receipt.AvgTable = cfg.Receipt.Turnover / cfg.Receipt.OpenTableCount
	}
	// 人均消费 = 营业额 ÷ 客流量
	if cfg.Receipt.GuestFlow > 0 {
		cfg.Receipt.AvgPerson = cfg.Receipt.Turnover / cfg.Receipt.GuestFlow
	}

	logStep("🧠", "渲染模板...")
	rendered, err := renderTemplate(string(templateText), cfg)
	if err != nil {
		fail("模板渲染失败", err)
		pause("按 Enter 退出")
		return
	}

	baseLogs := []string{
		color("\x1b[36m", bannerText),
		"",
		color("\x1b[90m", "•") + " 🧾 读取配置成功",
		color("\x1b[90m", "•") + " 🧩 读取模板成功",
		color("\x1b[90m", "•") + " 🧠 渲染模板成功",
	}

	selectedPrinter := ""
	for {
		if selectedPrinter == "" {
			printers, err := escpos.GetInstalledPrinters()
			if err != nil {
				fail("读取打印机列表失败", err)
				pause("按 Enter 重试")
				continue
			}
			if len(printers) == 0 {
				fail("未找到任何已安装打印机", errors.New("请先安装打印机驱动，并在“设备和打印机”中可见"))
				pause("按 Enter 重试")
				continue
			}
			defaultPrinter, _ := getDefaultPrinterName()
			sp, err := selectPrinter(printers, defaultPrinter, baseLogs)
			if err != nil {
				if errors.Is(err, errUserCancel) {
					return
				}
				fail("未选择打印机", err)
				pause("按 Enter 返回")
				continue
			}
			selectedPrinter = sp
		}

		runLogs := append([]string{}, baseLogs...)
		runLogs = append(runLogs, "",
			color("\x1b[90m", "•")+" 🔌 连接打印机: "+selectedPrinter)
		printPage(runLogs)

		printer, err := escpos.NewWindowsPrinter(selectedPrinter)
		if err != nil {
			runLogs = append(runLogs, color("\x1b[31m", "✗ 连接失败: "+err.Error()))
			choice := showPostActions(runLogs, selectedPrinter)
			switch choice {
			case actionRetry:
				continue
			case actionSwitch:
				selectedPrinter = ""
				continue
			case actionExit:
				return
			}
		}
		defer func() { _ = printer.Close() }()

		runLogs = append(runLogs, color("\x1b[90m", "•")+" 🧾 发送打印指令中...")
		printPage(runLogs)

		printErr := printRendered(printer, rendered, cfg.Printer.Encoding)
		if closeErr := printer.Close(); closeErr != nil && printErr == nil {
			printErr = closeErr
		}
		if printErr != nil {
			runLogs = append(runLogs, color("\x1b[31m", "✗ 打印失败: "+printErr.Error()))
			choice := showPostActions(runLogs, selectedPrinter)
			switch choice {
			case actionRetry:
				continue
			case actionSwitch:
				selectedPrinter = ""
				continue
			case actionExit:
				return
			}
		}

		runLogs = append(runLogs, color("\x1b[32m", "✅ 已提交到打印队列"))
		choice := showPostActions(runLogs, selectedPrinter)
		switch choice {
		case actionRetry:
			continue
		case actionSwitch:
			selectedPrinter = ""
			continue
		case actionExit:
			return
		}
	}
}

// exeDir 返回当前可执行文件所在目录，用于定位同目录下的 config.yaml / template。
func exeDir() (string, error) {
	exePath, err := os.Executable()
	if err != nil {
		return "", err
	}
	exePath, err = filepath.EvalSymlinks(exePath)
	if err != nil {
		return "", err
	}
	return filepath.Dir(exePath), nil
}

// readConfig 读取并解析 YAML 配置。
func readConfig(path string) (Config, error) {
	var cfg Config
	b, err := os.ReadFile(path)
	if err != nil {
		return cfg, err
	}
	if err := yaml.Unmarshal(b, &cfg); err != nil {
		return cfg, err
	}
	return cfg, nil
}

const (
	stdInputHandle  = uintptr(^uint32(9))
	stdOutputHandle = uintptr(^uint32(10))
)

const (
	enableVirtualTerminalProcessing = 0x0004
	enableLineInput                = 0x0002
	enableEchoInput                = 0x0004
)

const (
	keyEvent = 0x0001
)

const (
	vkUp     = 0x26
	vkDown   = 0x28
	vkReturn = 0x0D
	vkEscape = 0x1B
	vkC      = 0x43
	vkR      = 0x52
)

type keyEventRecord struct {
	KeyDown          int32
	RepeatCount      uint16
	VirtualKeyCode   uint16
	VirtualScanCode  uint16
	UnicodeChar      uint16
	ControlKeyState  uint32
}

type inputRecord struct {
	EventType uint16
	_         uint16
	KeyEvent  keyEventRecord
}

// getDefaultPrinterName 通过 WinAPI 获取系统默认打印机名称（GetDefaultPrinterW）。
func getDefaultPrinterName() (string, error) {
	winspool := syscall.NewLazyDLL("winspool.drv")
	proc := winspool.NewProc("GetDefaultPrinterW")

	var needed uint32
	r1, _, e1 := proc.Call(0, uintptr(unsafe.Pointer(&needed)))
	if r1 == 0 {
		if e1 != nil && e1 != syscall.ERROR_INSUFFICIENT_BUFFER {
			return "", e1
		}
	}
	if needed == 0 {
		return "", errors.New("默认打印机为空")
	}

	buf := make([]uint16, needed)
	r2, _, e2 := proc.Call(uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&needed)))
	if r2 == 0 {
		if e2 != nil {
			return "", e2
		}
		return "", errors.New("读取默认打印机失败")
	}

	s := syscall.UTF16ToString(buf)
	return strings.TrimSpace(s), nil
}

// selectPrinter 在终端中让用户选择要使用的打印机：
// - headerLines 会在每次刷新时打印在打印机列表上方（可为 nil）
// - 首选交互模式：↑↓移动，Enter确认，Esc取消（VT + ReadConsoleInputW）
// - 若当前控制台不支持，则回退为输入序号选择
func selectPrinter(printers []string, defaultName string, headerLines []string) (string, error) {
	restoreIn, inOK := enableDirectKeyInput()
	if restoreIn != nil {
		defer restoreIn()
	}

	initial := 0
	if strings.TrimSpace(defaultName) != "" {
		for i, p := range printers {
			if p == defaultName {
				initial = i
				break
			}
		}
	}

	if !vtEnabled || !inOK {
		fmt.Println()
		fmt.Println("可用打印机列表：")
		for i, p := range printers {
			suffix := ""
			if p == defaultName && defaultName != "" {
				suffix = "【默认打印机】"
			}
			if suffix != "" {
				suffix = " " + suffix
			}
			fmt.Printf("%2d) %s%s\n", i+1, p, suffix)
		}
		fmt.Println()
		fmt.Print("请输入序号并回车：")
		var n int
		_, err := fmt.Scanln(&n)
		if err != nil {
			return "", err
		}
		if n < 1 || n > len(printers) {
			return "", fmt.Errorf("无效序号: %d", n)
		}
		return printers[n-1], nil
	}

	selected := initial
	redraw := func() {
		fmt.Print("\x1b[2J\x1b[H")
		for _, h := range headerLines {
			fmt.Println(h)
		}
		fmt.Println()
		fmt.Println(color("\x1b[37m", "请选择要使用的打印机（↑↓移动，Enter确认，Esc退出）："))
		fmt.Println()
		for i, p := range printers {
			suffix := ""
			if p == defaultName && defaultName != "" {
				suffix = " " + color("\x1b[36m", "【默认打印机】")
			}
			line := fmt.Sprintf("%2d) %s%s", i+1, p, suffix)
			if i == selected {
				fmt.Print("\x1b[32m")
				fmt.Print("➤ ")
				fmt.Print(line)
				fmt.Print("\x1b[0m")
				fmt.Println()
			} else {
				fmt.Print("  ")
				fmt.Println(line)
			}
		}
	}

	redraw()

	for {
		vk, ok, err := readVirtualKey()
		if err != nil {
			return "", err
		}
		if !ok {
			continue
		}
		switch vk {
		case vkUp:
			if selected > 0 {
				selected--
				redraw()
			}
		case vkDown:
			if selected < len(printers)-1 {
				selected++
				redraw()
			}
		case vkReturn:
			fmt.Print("\x1b[0m")
			return printers[selected], nil
		case vkEscape:
			return "", errUserCancel
		}
	}
}

// enableVTOutput 打开控制台的 VT 转义支持（用于清屏、光标控制、颜色高亮）。
func enableVTOutput() (func(), bool) {
	kernel32 := syscall.NewLazyDLL("kernel32.dll")
	getStdHandle := kernel32.NewProc("GetStdHandle")
	getConsoleMode := kernel32.NewProc("GetConsoleMode")
	setConsoleMode := kernel32.NewProc("SetConsoleMode")

	h, _, _ := getStdHandle.Call(stdOutputHandle)
	if h == 0 {
		return nil, false
	}

	var original uint32
	r1, _, _ := getConsoleMode.Call(h, uintptr(unsafe.Pointer(&original)))
	if r1 == 0 {
		return nil, false
	}

	updated := original | enableVirtualTerminalProcessing
	r2, _, _ := setConsoleMode.Call(h, uintptr(updated))
	if r2 == 0 {
		return nil, false
	}

	restore := func() {
		_, _, _ = setConsoleMode.Call(h, uintptr(original))
	}
	return restore, true
}

// enableDirectKeyInput 关闭行缓冲与回显，使方向键等按键可以被 ReadConsoleInputW 逐个读取。
func enableDirectKeyInput() (func(), bool) {
	kernel32 := syscall.NewLazyDLL("kernel32.dll")
	getStdHandle := kernel32.NewProc("GetStdHandle")
	getConsoleMode := kernel32.NewProc("GetConsoleMode")
	setConsoleMode := kernel32.NewProc("SetConsoleMode")

	h, _, _ := getStdHandle.Call(stdInputHandle)
	if h == 0 {
		return nil, false
	}

	var original uint32
	r1, _, _ := getConsoleMode.Call(h, uintptr(unsafe.Pointer(&original)))
	if r1 == 0 {
		return nil, false
	}

	updated := original &^ (enableLineInput | enableEchoInput)
	r2, _, _ := setConsoleMode.Call(h, uintptr(updated))
	if r2 == 0 {
		return nil, false
	}

	restore := func() {
		_, _, _ = setConsoleMode.Call(h, uintptr(original))
	}
	return restore, true
}

// readVirtualKey 从控制台输入读取一次 KeyDown 事件并返回 VirtualKeyCode。
// 返回 ok=false 表示本次事件不是“按下键”的 KeyEvent（例如 KeyUp 或其他事件）。
func readVirtualKey() (uint16, bool, error) {
	kernel32 := syscall.NewLazyDLL("kernel32.dll")
	getStdHandle := kernel32.NewProc("GetStdHandle")
	readConsoleInput := kernel32.NewProc("ReadConsoleInputW")

	h, _, _ := getStdHandle.Call(stdInputHandle)
	if h == 0 {
		return 0, false, errors.New("无法读取控制台输入句柄")
	}

	var rec inputRecord
	var read uint32
	r1, _, e1 := readConsoleInput.Call(
		h,
		uintptr(unsafe.Pointer(&rec)),
		1,
		uintptr(unsafe.Pointer(&read)),
	)
	if r1 == 0 {
		if e1 != nil {
			return 0, false, e1
		}
		return 0, false, errors.New("读取键盘输入失败")
	}
	if read != 1 {
		return 0, false, nil
	}
	if rec.EventType != keyEvent {
		return 0, false, nil
	}
	if rec.KeyEvent.KeyDown == 0 {
		return 0, false, nil
	}
	return rec.KeyEvent.VirtualKeyCode, true, nil
}

const markerDelim = "\x1e"

// renderTemplate 使用 text/template 渲染 template：
// - 通过 FuncMap 注入控制标记（CENTER/BOLD/RESET/FEED/CUT 等）
// - 模板输出仍是“文本 + 标记”的单一字符串，后续由 printRendered 逐行解析并下发到打印机
func renderTemplate(tpl string, cfg Config) (string, error) {
	funcMap := template.FuncMap{
		"center": func() string { return markerDelim + "CENTER" + markerDelim },
		"left":   func() string { return markerDelim + "LEFT" + markerDelim },
		"right":  func() string { return markerDelim + "RIGHT" + markerDelim },
		"bold":   func() string { return markerDelim + "BOLD" + markerDelim },
		"reset":  func() string { return markerDelim + "RESET" + markerDelim },
		"feed": func(n int) string {
			return markerDelim + "FEED:" + strconv.Itoa(n) + markerDelim
		},
		"cut": func() string { return markerDelim + "CUT" + markerDelim },
		"fontsize": func(w, h int) string {
			return markerDelim + "FONTSIZE:" + strconv.Itoa(w) + ":" + strconv.Itoa(h) + markerDelim
		},
		"amt": func(v any) string { return formatAmount(v) },
		"num": func(v any) string { return formatNumber(v, false) },
		"pct": func(v any) string { return formatPercent(v) },
		"rjust": func(v any) string {
			return padLeftByWidth(formatNumber(v, true), 12)
		},
		"lr": func(left any, right any) string {
			l := fmt.Sprint(left)
			r := fmt.Sprint(right)
			return joinLeftRight(l, r, cfg.Printer.Columns)
		},
		"ld": func(left any, right any) string {
			l := fmt.Sprint(left)
			r := fmt.Sprint(right)
			return joinLeftRight(l, r, cfg.Printer.Columns-12)
		},
		"hr": func(ch string) string {
			if ch == "" {
				ch = "-"
			}
			return strings.Repeat(ch, cfg.Printer.Columns)
		},
	}

	t, err := template.New("receipt").Funcs(funcMap).Option("missingkey=error").Parse(tpl)
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, cfg.Receipt); err != nil {
		return "", err
	}
	s := buf.String()
	s = strings.ReplaceAll(s, "\r\n", "\n")
	return s, nil
}

var feedRe = regexp.MustCompile(`^FEED:(\d+)$`)
var fontSizeRe = regexp.MustCompile(`^FONTSIZE:(\d+):(\d+)$`)

// printRendered 解析 renderTemplate 的输出并打印：
// - 按行处理，行内用 markerDelim 切分控制标记
// - 标记会改变后续文本的样式（对齐/加粗），或触发走纸/切纸
func printRendered(p escpos.Printer, rendered string, encoding string) error {
	if err := p.Initialize(); err != nil {
		return err
	}
	_ = p.SetLineSpacing(100)
	lines := strings.Split(rendered, "\n")

	for _, line := range lines {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			if err := p.LF(); err != nil {
				return err
			}
			continue
		}

		align := escpos.LeftJustify
		bold := false
		fontW, fontH := 0, 0
		printedText := false

		writeText := func(s string) error {
			if s == "" {
				return nil
			}
			printedText = true
			if err := p.Justify(align); err != nil {
				return err
			}
			if err := p.SetBold(bold); err != nil {
				return err
			}
			if err := p.SetCharacterSize(fontW, fontH); err != nil {
				return err
			}
			if err := writePrinterText(p, s, encoding); err != nil {
				return err
			}
			return nil
		}

		i := 0
		for {
			j := strings.Index(line[i:], markerDelim)
			if j < 0 {
				if err := writeText(line[i:]); err != nil {
					return err
				}
				break
			}
			j = i + j
			if err := writeText(line[i:j]); err != nil {
				return err
			}
			k := strings.Index(line[j+len(markerDelim):], markerDelim)
			if k < 0 {
				if err := writeText(line[j:]); err != nil {
					return err
				}
				break
			}
			k = j + len(markerDelim) + k
			token := line[j+len(markerDelim) : k]

			switch token {
			case "CENTER":
				align = escpos.CenterJustify
			case "LEFT":
				align = escpos.LeftJustify
			case "RIGHT":
				align = escpos.RightJustify
			case "BOLD":
				bold = true
			case "RESET":
				align = escpos.LeftJustify
				bold = false
				fontW, fontH = 0, 0
			case "CUT":
				if err := p.Justify(escpos.LeftJustify); err != nil {
					return err
				}
				if err := p.SetBold(false); err != nil {
					return err
				}
				if err := p.Cut(); err != nil {
					return err
				}
			default:
				if m := feedRe.FindStringSubmatch(token); len(m) == 2 {
					n, _ := strconv.Atoi(m[1])
					if n > 0 {
						if err := p.FeedLines(n); err != nil {
							return err
						}
					}
				}
				if m := fontSizeRe.FindStringSubmatch(token); len(m) == 3 {
					w, _ := strconv.Atoi(m[1])
					h, _ := strconv.Atoi(m[2])
					if w > 0 {
						fontW = w
					}
					if h > 0 {
						fontH = h
					}
				}
			}

			i = k + len(markerDelim)
		}

		if printedText {
			if err := p.LF(); err != nil {
				return err
			}
		}
		if err := p.SetBold(false); err != nil {
			return err
		}
		if err := p.Justify(escpos.LeftJustify); err != nil {
			return err
		}
	}
	return nil
}

// formatAmount 将数值格式化为千分位 + 两位小数（用于金额展示）。
func formatAmount(v any) string {
	return formatNumber(v, true)
}

// formatPercent 将数值格式化为两位小数百分比（例如 208.82%）。
func formatPercent(v any) string {
	f, ok := toFloat64(v)
	if !ok {
		s := strings.TrimSpace(fmt.Sprint(v))
		if s == "" {
			return ""
		}
		return s
	}
	return fmt.Sprintf("%.2f%%", f)
}

// formatNumber 将数值格式化为两位小数；useComma=true 时整数部分加千分位分隔符。
func formatNumber(v any, useComma bool) string {
	f, ok := toFloat64(v)
	if !ok {
		s := strings.TrimSpace(fmt.Sprint(v))
		if s == "" {
			return ""
		}
		return s
	}

	s := fmt.Sprintf("%.2f", f)
	if !useComma {
		return s
	}
	parts := strings.SplitN(s, ".", 2)
	intPart := parts[0]
	neg := strings.HasPrefix(intPart, "-")
	if neg {
		intPart = strings.TrimPrefix(intPart, "-")
	}

	var out []byte
	for i, r := range intPart {
		if i > 0 && (len(intPart)-i)%3 == 0 {
			out = append(out, ',')
		}
		out = append(out, byte(r))
	}
	if neg {
		out = append([]byte{'-'}, out...)
	}
	if len(parts) == 2 {
		out = append(out, '.')
		out = append(out, parts[1]...)
	}
	return string(out)
}

// toFloat64 将常见数值类型/字符串转换成 float64，模板中可直接传入 int/float/string。
func toFloat64(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case float32:
		return float64(x), true
	case int:
		return float64(x), true
	case int8:
		return float64(x), true
	case int16:
		return float64(x), true
	case int32:
		return float64(x), true
	case int64:
		return float64(x), true
	case uint:
		return float64(x), true
	case uint8:
		return float64(x), true
	case uint16:
		return float64(x), true
	case uint32:
		return float64(x), true
	case uint64:
		return float64(x), true
	case string:
		s := strings.TrimSpace(x)
		if s == "" {
			return 0, false
		}
		f, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return 0, false
		}
		return f, true
	default:
		return 0, false
	}
}

// joinLeftRight 将 left/right 拼成一行并补空格，使两端对齐（按 columns 与 displayWidth 计算）。
func joinLeftRight(left, right string, columns int) string {
	lw := displayWidth(left)
	rw := displayWidth(right)
	space := columns - lw - rw
	if space < 1 {
		space = 1
	}
	return left + strings.Repeat(" ", space) + right
}

// padLeftByWidth 按显示宽度把字符串左侧补空格到指定宽度（用于右对齐数字）。
func padLeftByWidth(s string, width int) string {
	w := displayWidth(s)
	if w >= width {
		return s
	}
	return strings.Repeat(" ", width-w) + s
}

// displayWidth 估算控制台/小票的等宽显示宽度：ASCII=1，非 ASCII=2（适用于常见中文等宽打印效果）。
func displayWidth(s string) int {
	w := 0
	for _, r := range s {
		if r <= 0x7f {
			w++
			continue
		}
		w += 2
	}
	return w
}

// writePrinterText 按 printer.encoding 编码并写入打印机：
// - utf-8：直接调用 p.Print（库内部会写入字节）
// - gb18030：将字符串转码后写入原始字节（解决部分机型中文乱码）
func writePrinterText(p escpos.Printer, s string, encoding string) error {
	switch strings.ToLower(strings.TrimSpace(encoding)) {
	case "", "utf-8", "utf8":
		return p.Print(s)
	case "gb18030":
		out, _, err := transform.String(simplifiedchinese.GB18030.NewEncoder(), s)
		if err != nil {
			return err
		}
		_, err = p.Write([]byte(out))
		return err
	default:
		return fmt.Errorf("不支持的 printer.encoding: %s", encoding)
	}
}

func readBanner(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	s := strings.ReplaceAll(string(b), "\r\n", "\n")
	return strings.TrimRight(s, "\n"), nil
}

func printPage(lines []string) {
	if vtEnabled {
		fmt.Print("\x1b[2J\x1b[H")
	}
	for _, l := range lines {
		fmt.Println(l)
	}
}

type postAction int

const (
	actionRetry  postAction = iota
	actionSwitch
	actionExit
)

func showPostActions(logs []string, printerName string) postAction {
	logs = append(logs, "")
	logs = append(logs, color("\x1b[36m", "当前打印机: "+printerName))
	logs = append(logs, "")
	logs = append(logs, color("\x1b[1;37m", "  [Enter] 再次打印  ")+color("\x1b[90m", "用当前打印机再打一份"))
	logs = append(logs, color("\x1b[1;37m", "  [  C  ] 更改打印机")+color("\x1b[90m", "  切换到其他打印机"))
	logs = append(logs, color("\x1b[1;37m", "  [ Esc ] 退出程序  "))
	printPage(logs)

	restoreIn, _ := enableDirectKeyInput()
	if restoreIn != nil {
		defer restoreIn()
	}
	for {
		vk, ok, _ := readVirtualKey()
		if !ok {
			continue
		}
		switch vk {
		case vkReturn, vkR:
			return actionRetry
		case vkC:
			return actionSwitch
		case vkEscape:
			return actionExit
		}
	}
}

func logStep(icon, msg string) {
	fmt.Println(color("\x1b[90m", "•")+" "+icon+" "+msg)
}

func fail(title string, err error) {
	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, color("\x1b[31m", "✗ "+title))
	fmt.Fprintln(os.Stderr, color("\x1b[90m", err.Error()))
}

func pause(hint string) {
	fmt.Println()
	if strings.TrimSpace(hint) == "" {
		hint = "按 Enter 继续"
	}
	fmt.Println(color("\x1b[90m", hint))
	_, _ = bufio.NewReader(os.Stdin).ReadString('\n')
}

func color(code string, s string) string {
	if !vtEnabled {
		return s
	}
	return code + s + "\x1b[0m"
}
