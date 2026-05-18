package main

import (
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
	Name     string `yaml:"name"`
	Columns  int    `yaml:"columns"`
	Encoding string `yaml:"encoding"`
}

type Receipt struct {
	StoreName   string `yaml:"store_name"`
	StartTime   string `yaml:"start_time"`
	EndTime     string `yaml:"end_time"`
	Currency    string `yaml:"currency"`
	Area        string `yaml:"area"`
	Status      string `yaml:"status"`
	PrintPerson string `yaml:"print_person"`
	PrintTime   string `yaml:"print_time"`

	Turnover           float64 `yaml:"turnover"`
	ItemConsumption    float64 `yaml:"item_consumption"`
	ServiceFee         float64 `yaml:"service_fee"`
	MinConsumptionFill float64 `yaml:"min_consumption_fill"`
	DiscountTotal      float64 `yaml:"discount_total"`
	MemberDiscount     float64 `yaml:"member_discount"`
	PromotionDiscount  float64 `yaml:"promotion_discount"`
	GiftDiscount       float64 `yaml:"gift_discount"`
	DiscountAmount     float64 `yaml:"discount_amount"`
	FixedDiscount      float64 `yaml:"fixed_discount"`
	Rounding           float64 `yaml:"rounding"`
	PaymentDiscount    float64 `yaml:"payment_discount"`

	ActualAmount   float64 `yaml:"actual_amount"`
	WechatPay      float64 `yaml:"wechat_pay"`
	WechatSubsidy  float64 `yaml:"wechat_subsidy"`
	Cash           float64 `yaml:"cash"`
	Alipay         float64 `yaml:"alipay"`
	MeituanGroup   float64 `yaml:"meituan_group"`
	MeituanWaimai  float64 `yaml:"meituan_waimai"`
	BillCount      float64 `yaml:"bill_count"`
	OpenTableCount float64 `yaml:"open_table_count"`
	GuestFlow      float64 `yaml:"guest_flow"`
	AvgBill        float64 `yaml:"avg_bill"`
	AvgTable       float64 `yaml:"avg_table"`
	AvgPerson      float64 `yaml:"avg_person"`
	OpenTableRate  float64 `yaml:"open_table_rate"`
	SeatRate       float64 `yaml:"seat_rate"`
	TurnoverRate   float64 `yaml:"turnover_rate"`
	AvgDiningTime  float64 `yaml:"avg_dining_time"`
	AreaEff        float64 `yaml:"area_eff"`
	PersonEff      float64 `yaml:"person_eff"`
}

// main 是程序入口：
// 读取 config.yaml / template.txt → 让用户选择 Windows 打印机 → 渲染模板并发送 ESC/POS 指令打印 → 弹窗提示结果。
func main() {
	fmt.Println("正在读取配置...")
	baseDir, err := exeDir()
	if err != nil {
		fatalWithBox("无法获取程序目录", err)
	}

	cfg, err := readConfig(filepath.Join(baseDir, "config.yaml"))
	if err != nil {
		fatalWithBox("读取 config.yaml 失败", err)
	}

	if cfg.Printer.Columns <= 0 {
		cfg.Printer.Columns = 48
	}
	if strings.TrimSpace(cfg.Printer.Encoding) == "" {
		cfg.Printer.Encoding = "utf-8"
	}

	if cfg.Receipt.ActualAmount == 0 {
		cfg.Receipt.ActualAmount = cfg.Receipt.WechatPay + cfg.Receipt.WechatSubsidy + cfg.Receipt.Cash + cfg.Receipt.Alipay + cfg.Receipt.MeituanGroup + cfg.Receipt.MeituanWaimai
	}

	templateText, err := os.ReadFile(filepath.Join(baseDir, "template.txt"))
	if err != nil {
		fatalWithBox("读取 template.txt 失败", err)
	}

	rendered, err := renderTemplate(string(templateText), cfg)
	if err != nil {
		fatalWithBox("模板渲染失败", err)
	}

	printers, err := escpos.GetInstalledPrinters()
	if err != nil {
		fatalWithBox("读取打印机列表失败", err)
	}
	if len(printers) == 0 {
		fatalWithBox("未找到任何已安装打印机", errors.New("请先安装打印机驱动，并在“设备和打印机”中可见"))
	}
	defaultPrinter, _ := getDefaultPrinterName()
	selectedPrinter, err := selectPrinter(printers, defaultPrinter, cfg.Printer.Name)
	if err != nil {
		fatalWithBox("未选择打印机", err)
	}
	cfg.Printer.Name = selectedPrinter

	fmt.Println("正在连接打印机:", cfg.Printer.Name)
	printer, err := escpos.NewWindowsPrinter(cfg.Printer.Name)
	if err != nil {
		fatalWithBox("未找到打印机："+cfg.Printer.Name, err)
	}
	defer func() { _ = printer.Close() }()

	fmt.Println("正在打印...")
	if err := printRendered(printer, rendered, cfg.Printer.Encoding); err != nil {
		fatalWithBox("打印失败", err)
	}

	infoBox("打印完成")
}

// exeDir 返回当前可执行文件所在目录，用于定位同目录下的 config.yaml / template.txt。
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

// readConfig 读取并解析 YAML 配置；printer.name 允许留空（启动后会交互选择打印机）。
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
// - 首选交互模式：↑↓移动，Enter确认，Esc取消（VT + ReadConsoleInputW）
// - 若当前控制台不支持，则回退为输入序号选择
// preferred 用于定位初始光标（一般来自 config 的 printer.name），其次使用系统默认打印机。
func selectPrinter(printers []string, defaultName, preferred string) (string, error) {
	restoreOut, vtOK := enableVTOutput()
	if restoreOut != nil {
		defer restoreOut()
	}

	restoreIn, inOK := enableDirectKeyInput()
	if restoreIn != nil {
		defer restoreIn()
	}

	initial := 0
	if strings.TrimSpace(preferred) != "" {
		for i, p := range printers {
			if p == preferred {
				initial = i
				break
			}
		}
	} else if strings.TrimSpace(defaultName) != "" {
		for i, p := range printers {
			if p == defaultName {
				initial = i
				break
			}
		}
	}

	fmt.Println()
	fmt.Println("可用打印机列表：")
	for i, p := range printers {
		suffix := ""
		if p == defaultName && defaultName != "" {
			suffix = " (默认打印机)"
		}
		fmt.Printf("%2d) %s%s\n", i+1, p, suffix)
	}
	fmt.Println()

	if !vtOK || !inOK {
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
		fmt.Println("请选择要使用的打印机（↑↓移动，Enter确认，Esc取消）：")
		fmt.Println()
		for i, p := range printers {
			suffix := ""
			if p == defaultName && defaultName != "" {
				suffix = " (默认打印机)"
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
			fmt.Print("\x1b[2J\x1b[H")
			return printers[selected], nil
		case vkEscape:
			return "", errors.New("用户取消")
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

// renderTemplate 使用 text/template 渲染 template.txt：
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

// printRendered 解析 renderTemplate 的输出并打印：
// - 按行处理，行内用 markerDelim 切分控制标记
// - 标记会改变后续文本的样式（对齐/加粗），或触发走纸/切纸
func printRendered(p escpos.Printer, rendered string, encoding string) error {
	if err := p.Initialize(); err != nil {
		return err
	}
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
			}

			i = k + len(markerDelim)
		}

		if err := p.SetBold(false); err != nil {
			return err
		}
		if err := p.Justify(escpos.LeftJustify); err != nil {
			return err
		}
		if printedText {
			if err := p.LF(); err != nil {
				return err
			}
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

// fatalWithBox 在终端输出错误并弹窗提示后退出进程（exit code=1）。
func fatalWithBox(title string, err error) {
	fmt.Fprintln(os.Stderr, title+":", err)
	errorBox(title + "\n\n" + err.Error())
	os.Exit(1)
}

// infoBox 弹出信息提示框（MessageBoxW, MB_ICONINFORMATION）。
func infoBox(message string) {
	messageBox("提示", message, 0x00000040)
}

// errorBox 弹出错误提示框（MessageBoxW, MB_ICONERROR）。
func errorBox(message string) {
	messageBox("错误", message, 0x00000010)
}

// messageBox 封装 WinAPI MessageBoxW，用于在无 GUI 的情况下提示结果/错误。
func messageBox(title, text string, flags uintptr) {
	user32 := syscall.NewLazyDLL("user32.dll")
	proc := user32.NewProc("MessageBoxW")
	textPtr, _ := syscall.UTF16PtrFromString(text)
	titlePtr, _ := syscall.UTF16PtrFromString(title)
	_, _, _ = proc.Call(
		0,
		uintptr(unsafe.Pointer(textPtr)),
		uintptr(unsafe.Pointer(titlePtr)),
		flags,
	)
}
