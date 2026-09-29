package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	port            = 2233
	maxJSONBodySize = 10 << 20
	ffmpegTimeout   = 15 * time.Second
	cleanupInterval = 5 * time.Minute
	cleanupMaxAge   = 5 * time.Minute
)

var (
	imageDataURLPattern = regexp.MustCompile(`^data:(image/[a-zA-Z0-9.+-]+);base64,(.+)$`)
	safeColorPattern    = regexp.MustCompile(`^0x[0-9A-Fa-f]{6}$`)
	safeMaterialPattern = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)
	tempRoot            string
	imageRoot           string
	ffmpegPath          string
)

type generateRequest struct {
	Image       string      `json:"image"`
	Color       string      `json:"color"`
	TargetColor string      `json:"targetColor"` // 新增目标颜色字段
	Similarity  numberValue `json:"similarity"`
	Blend       numberValue `json:"blend"`
}

type synthesisRequest struct {
	Image    string      `json:"image"`
	X        numberValue `json:"x"`
	Y        numberValue `json:"y"`
	Rotate   numberValue `json:"rotate"`
	Material string      `json:"material"`
}

type numberValue struct {
	set   bool
	value float64
}

func main() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)

	baseDir, err := getBaseDir()
	if err != nil {
		log.Fatalf("❌ 获取程序目录失败: %v", err)
	}

	tempRoot = filepath.Join(baseDir, "temp")
	imageRoot = filepath.Join(baseDir, "images")

	if err := os.MkdirAll(tempRoot, 0o755); err != nil {
		log.Fatalf("❌ 创建临时文件根目录失败: %v", err)
	}
	log.Printf("📂 临时文件根目录: %s", tempRoot)

	if err := os.MkdirAll(imageRoot, 0o755); err != nil {
		log.Fatalf("❌ 创建合成图片根目录失败: %v", err)
	}
	log.Printf("📂 合成图片根目录: %s", imageRoot)

	ffmpegPath, err = resolveFFmpegPath(baseDir)
	if err != nil {
		log.Fatalf("❌ 未找到 ffmpeg 可执行文件: %v", err)
	}
	log.Printf("🎬 FFmpeg 可执行文件: %s", ffmpegPath)

	startCleanupTicker(tempRoot, cleanupMaxAge)
	startCleanupTicker(imageRoot, cleanupMaxAge)

	mux := http.NewServeMux()
	mux.HandleFunc("/ffmpeg/generate", generateHandler)
	mux.HandleFunc("/ffmpeg/replaceColor", replaceColor)
	mux.HandleFunc("/ffmpeg/synthesis", synthesisHandler)

	server := &http.Server{
		Addr:              fmt.Sprintf(":%d", port),
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       20 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	log.Printf("🚀 Server running at http://localhost:%d", port)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("❌ 启动服务器失败: %v", err)
	}
}

// -------------------------
// 工具函数
// -------------------------

func getBaseDir() (string, error) {
	wd, err := os.Getwd()
	if err == nil {
		if _, statErr := os.Stat(filepath.Join(wd, "go.mod")); statErr == nil {
			return wd, nil
		}
	}

	exePath, exeErr := os.Executable()
	if exeErr != nil {
		return "", exeErr
	}
	return filepath.Dir(exePath), nil
}

func getCurrentTimeDir() string {
	return time.Now().Format("200601021504")
}

func newRequestID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 10)
	}
	return hex.EncodeToString(b[:])
}

func saveBase64Image(base64Payload, filePath string) error {
	log.Printf("💾 保存 Base64 图片到: %s", filePath)

	buffer, err := base64.StdEncoding.DecodeString(base64Payload)
	if err != nil {
		return fmt.Errorf("Base64 图片解码失败: %w", err)
	}

	if err := os.WriteFile(filePath, buffer, 0o644); err != nil {
		return fmt.Errorf("写入图片文件失败: %w", err)
	}

	return nil
}

func resolveFFmpegPath(baseDir string) (string, error) {
	candidates := []string{
		filepath.Join(baseDir, "ffmpeg.exe"),
		filepath.Join(baseDir, "ffmpeg"),
		filepath.Join(".", "ffmpeg.exe"),
		filepath.Join(".", "ffmpeg"),
	}

	for _, candidate := range candidates {
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate, nil
		}
	}

	pathFromEnv, err := exec.LookPath("ffmpeg")
	if err != nil {
		return "", err
	}
	return pathFromEnv, nil
}

func runExecCmd(args []string) error {
	log.Printf("▶ 执行合成命令: %s %s", ffmpegPath, strings.Join(args, " "))

	ctx, cancel := context.WithTimeout(context.Background(), ffmpegTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, ffmpegPath, args...)
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	if out := strings.TrimSpace(stdout.String()); out != "" {
		log.Printf("📄 stdout: %s", out)
	}
	if errText := strings.TrimSpace(stderr.String()); errText != "" {
		log.Printf("⚠ stderr: %s", errText)
	}

	if ctx.Err() == context.DeadlineExceeded {
		log.Printf("❌ 合成命令执行超时")
		return fmt.Errorf("执行 ffmpeg 超时")
	}

	if err == nil {
		log.Printf("✅ 合成命令执行完成")
		return nil
	}
	log.Printf("❌ 合成命令执行失败: %v", err)
	return err
}

func cleanOldDirs(baseDir string, maxAge time.Duration) {
	entries, err := os.ReadDir(baseDir)
	if err != nil {
		log.Printf("❌ 清理旧目录失败: %v", err)
		return
	}

	now := time.Now()
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		fullPath := filepath.Join(baseDir, entry.Name())
		info, err := entry.Info()
		if err != nil {
			log.Printf("⚠ 获取目录信息失败，跳过: %s, err=%v", fullPath, err)
			continue
		}

		if now.Sub(info.ModTime()) <= maxAge {
			continue
		}

		if err := os.RemoveAll(fullPath); err != nil {
			log.Printf("⚠ 删除旧目录失败: %s, err=%v", fullPath, err)
			continue
		}

		log.Printf("🗑 删除旧目录: %s", fullPath)
	}
}

func startCleanupTicker(baseDir string, maxAge time.Duration) {
	cleanOldDirs(baseDir, maxAge)

	go func() {
		ticker := time.NewTicker(cleanupInterval)
		defer ticker.Stop()

		for range ticker.C {
			log.Printf("⏰ 定时清理旧目录任务启动")
			cleanOldDirs(baseDir, maxAge)
		}
	}()
}

func createTimeDir(root string) (string, error) {
	dir := filepath.Join(root, getCurrentTimeDir()+"_"+newRequestID())
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	log.Printf("📂 临时文件目录: %s", dir)
	return dir, nil
}

func parseBase64Image(image string) (ext string, rawBase64 string, err error) {
	if image == "" {
		return "", "", errors.New("没有提供图片")
	}

	ext = "png"
	rawBase64 = image

	match := imageDataURLPattern.FindStringSubmatch(image)
	if len(match) == 3 {
		mimeType := strings.ToLower(match[1])
		ext = mimeType[strings.LastIndex(mimeType, "/")+1:]
		ext = strings.ReplaceAll(ext, "+xml", "")
		rawBase64 = match[2]
	}

	return ext, rawBase64, nil
}

func (n *numberValue) UnmarshalJSON(data []byte) error {
	data = bytes.TrimSpace(data)
	if bytes.Equal(data, []byte("null")) || len(data) == 0 {
		return nil
	}

	var floatVal float64
	if err := json.Unmarshal(data, &floatVal); err == nil {
		n.set = true
		n.value = floatVal
		return nil
	}

	var stringVal string
	if err := json.Unmarshal(data, &stringVal); err == nil {
		stringVal = strings.TrimSpace(stringVal)
		if stringVal == "" {
			return nil
		}

		parsed, parseErr := strconv.ParseFloat(stringVal, 64)
		if parseErr != nil {
			return fmt.Errorf("数字字段格式不正确: %s", stringVal)
		}

		n.set = true
		n.value = parsed
		return nil
	}

	return errors.New("数字字段必须是数字或数字字符串")
}

func clamp(value, minValue, maxValue float64) float64 {
	if value < minValue {
		return minValue
	}
	if value > maxValue {
		return maxValue
	}
	return value
}

func formatFFmpegFloat(value float64) string {
	return strconv.FormatFloat(value, 'f', -1, 64)
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(payload); err != nil {
		log.Printf("❌ 响应写入失败: %v", err)
	}
}

func decodeJSONBody(w http.ResponseWriter, r *http.Request, dst any) error {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{
			"code": http.StatusMethodNotAllowed,
			"msg":  "请求方法不允许",
		})
		return errors.New("请求方法不允许")
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxJSONBodySize)
	defer r.Body.Close()

	decoder := json.NewDecoder(r.Body)
	if err := decoder.Decode(dst); err != nil {
		if errors.Is(err, io.EOF) {
			return errors.New("请求体不能为空")
		}
		return fmt.Errorf("JSON 解析失败: %w", err)
	}

	return nil
}

func buildSourceFile(timeDir, ext string) string {
	return filepath.Join(timeDir, newRequestID()+"."+ext)
}

// ======================================================================
// 抠图接口：上传 Base64 + 图片合成 + 返回最终图片 （一个接口）
// ======================================================================

func generateHandler(w http.ResponseWriter, r *http.Request) {
	log.Printf("📥 接收到透明抠图 + GIF 合成请求")

	var req generateRequest
	if err := decodeJSONBody(w, r, &req); err != nil {
		log.Printf("❌ 请求解析失败: %v", err)
		if !strings.Contains(err.Error(), "请求方法不允许") {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		}
		return
	}

	if strings.TrimSpace(req.Image) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "没有提供图片"})
		return
	}

	safeColor := "0xFEFEFE"
	if safeColorPattern.MatchString(req.Color) {
		safeColor = req.Color
	}
	similarity := 0.02
	if req.Similarity.set {
		similarity = req.Similarity.value
	}
	similarity = clamp(similarity, 0, 1)

	blend := 0.0
	if req.Blend.set {
		blend = req.Blend.value
	}
	blend = clamp(blend, 0, 1)

	timeDir, err := createTimeDir(tempRoot)
	if err != nil {
		log.Printf("❌ 创建临时目录失败: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"code": 500,
			"msg":  "ffmpeg合成服务器错误" + err.Error(),
		})
		return
	}

	ext, rawBase64, err := parseBase64Image(req.Image)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	srcFile := buildSourceFile(timeDir, ext)
	if err := saveBase64Image(rawBase64, srcFile); err != nil {
		log.Printf("❌ 保存临时图片失败: %v", err)
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	log.Printf("💾 保存临时图片: %s", srcFile)

	paletteFile := filepath.Join(timeDir, "palette.png")
	outputGIF := filepath.Join(timeDir, "output.gif")

	paletteArgs := []string{
		"-y",
		"-i", srcFile,
		"-vf", fmt.Sprintf("colorkey=%s:%s:%s,palettegen", safeColor, formatFFmpegFloat(similarity), formatFFmpegFloat(blend)),
		paletteFile,
	}
	if err := runExecCmd(paletteArgs); err != nil {
		log.Printf("❌ 合并接口失败: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"code": 500,
			"msg":  "ffmpeg合成服务器错误" + err.Error(),
		})
		return
	}

	gifArgs := []string{
		"-y",
		"-i", srcFile,
		"-i", paletteFile,
		"-lavfi",
		fmt.Sprintf("colorkey=%s:%s:%s [ck]; [ck][1:v] paletteuse", safeColor, formatFFmpegFloat(similarity), formatFFmpegFloat(blend)),
		outputGIF,
	}
	if err := runExecCmd(gifArgs); err != nil {
		log.Printf("❌ 合并接口失败: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"code": 500,
			"msg":  "ffmpeg合成服务器错误" + err.Error(),
		})
		return
	}

	buffer, err := os.ReadFile(outputGIF)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "合成失败：未生成 GIF 文件"})
		return
	}

	log.Printf("🎉 GIF 合成完成，返回 Base64")

	writeJSON(w, http.StatusOK, map[string]any{
		"code": 200,
		"msg":  "合成成功",
		"data": map[string]any{
			"ext":        "gif",
			"color":      safeColor,
			"similarity": similarity,
			"blend":      blend,
			"base64":     "data:image/gif;base64," + base64.StdEncoding.EncodeToString(buffer),
		},
	})
}

/**
 * 替换颜色
 */
func replaceColor(w http.ResponseWriter, r *http.Request) {
	log.Printf("📥 接收到替换颜色合成请求")

	var req generateRequest
	if err := decodeJSONBody(w, r, &req); err != nil {
		log.Printf("❌ 请求解析失败: %v", err)
		if !strings.Contains(err.Error(), "请求方法不允许") {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		}
		return
	}

	if strings.TrimSpace(req.Image) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "没有提供图片"})
		return
	}

	// 1. 解析要替换的源颜色（从 req.Color 中获取，默认纯红 255,0,0）
	srcR, srcG, srcB := 255, 0, 0
	safeColor := "0xFF0000"
	if safeColorPattern.MatchString(req.Color) {
		safeColor = req.Color
		srcR, srcG, srcB = parseHexColor(safeColor)
	}

	// 2. 解析替换后的目标颜色（若 req.TargetColor 未指定，默认替换为 0,119,255）
	targetR, targetG, targetB := 0, 119, 255
	if req.TargetColor != "" && safeColorPattern.MatchString(req.TargetColor) {
		targetR, targetG, targetB = parseHexColor(req.TargetColor)
	}

	timeDir, err := createTimeDir(tempRoot)
	if err != nil {
		log.Printf("❌ 创建临时目录失败: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"code": 500,
			"msg":  "ffmpeg合成服务器错误: " + err.Error(),
		})
		return
	}

	ext, rawBase64, err := parseBase64Image(req.Image)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	srcFile := buildSourceFile(timeDir, ext)
	if err := saveBase64Image(rawBase64, srcFile); err != nil {
		log.Printf("❌ 保存临时图片失败: %v", err)
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	log.Printf("💾 保存临时图片: %s", srcFile)

	// 输出路径（使用对应扩展名，默认为 jpg）
	outExt := ext
	if outExt == "" {
		outExt = "jpg"
	}
	outputFile := filepath.Join(timeDir, fmt.Sprintf("output.%s", outExt))

	// 构建 geq 滤镜表达式
	filterExpr := fmt.Sprintf(
		"format=rgb24,geq=r='if(eq(r(X,Y),%d)*eq(g(X,Y),%d)*eq(b(X,Y),%d),%d,r(X,Y))':g='if(eq(r(X,Y),%d)*eq(g(X,Y),%d)*eq(b(X,Y),%d),%d,g(X,Y))':b='if(eq(r(X,Y),%d)*eq(g(X,Y),%d)*eq(b(X,Y),%d),%d,b(X,Y))'",
		srcR, srcG, srcB, targetR,
		srcR, srcG, srcB, targetG,
		srcR, srcG, srcB, targetB,
	)

	// FFmpeg 命令配置
	ffmpegArgs := []string{
		"-y",
		"-i", srcFile,
		"-vf", filterExpr,
		"-q:v", "2", // 高画质输出
		outputFile,
	}

	if err := runExecCmd(ffmpegArgs); err != nil {
		log.Printf("❌ 合成接口失败: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"code": 500,
			"msg":  "ffmpeg合成服务器错误: " + err.Error(),
		})
		return
	}

	buffer, err := os.ReadFile(outputFile)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "合成失败：未生成图片文件"})
		return
	}

	log.Printf("🎉 图片替换颜色完成，返回 Base64")

	mimeType := "image/jpeg"
	if outExt == "png" {
		mimeType = "image/png"
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"code": 200,
		"msg":  "合成成功",
		"data": map[string]any{
			"ext":    outExt,
			"base64": fmt.Sprintf("data:%s;base64,%s", mimeType, base64.StdEncoding.EncodeToString(buffer)),
		},
	})
}

// 辅助函数：将 0xFF0000 或 #FF0000 等十六进制字符串转换为 R, G, B 数值
func parseHexColor(hexStr string) (int, int, int) {
	hexStr = strings.TrimPrefix(hexStr, "0x")
	hexStr = strings.TrimPrefix(hexStr, "0X")
	hexStr = strings.TrimPrefix(hexStr, "#")

	if len(hexStr) != 6 {
		return 255, 0, 0 // 默认返回红色
	}

	val, err := strconv.ParseInt(hexStr, 16, 64)
	if err != nil {
		return 255, 0, 0
	}

	r := int((val >> 16) & 0xFF)
	g := int((val >> 8) & 0xFF)
	b := int(val & 0xFF)
	return r, g, b
}

/**
 * 合成图接口
 */
func synthesisHandler(w http.ResponseWriter, r *http.Request) {
	log.Printf("📥 接收到素材图片合成请求")

	var req synthesisRequest
	if err := decodeJSONBody(w, r, &req); err != nil {
		log.Printf("❌ 请求解析失败: %v", err)
		if !strings.Contains(err.Error(), "请求方法不允许") {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		}
		return
	}

	if strings.TrimSpace(req.Image) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "没有提供图片"})
		return
	}
	if strings.TrimSpace(req.Material) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "没有提供素材图片"})
		return
	}
	if !safeMaterialPattern.MatchString(req.Material) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "素材图片名称不合法"})
		return
	}

	timeDir, err := createTimeDir(tempRoot)
	if err != nil {
		log.Printf("❌ 创建临时目录失败: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"code": 500,
			"msg":  "ffmpeg合成服务器错误" + err.Error(),
		})
		return
	}

	ext, rawBase64, err := parseBase64Image(req.Image)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	srcFile := buildSourceFile(timeDir, ext)
	if err := saveBase64Image(rawBase64, srcFile); err != nil {
		log.Printf("❌ 保存临时图片失败: %v", err)
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	log.Printf("💾 保存临时图片: %s", srcFile)

	outputFile := filepath.Join(timeDir, "output.png")

	materialExt := req.Material + ".png"
	materialSource := filepath.Join(imageRoot, materialExt)
	materialFile := filepath.Join(timeDir, materialExt)

	if err := copyMaterial(materialSource, materialFile); err != nil {
		log.Printf("❌ 复制素材图失败: %v", err)
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	log.Printf("🧩 素材已复制到: %s", materialFile)

	x := 300.0
	if req.X.set {
		x = req.X.value
	}
	y := 150.0
	if req.Y.set {
		y = req.Y.value
	}
	rotate := 0.0
	if req.Rotate.set {
		rotate = req.Rotate.value
	}

	pngArgs := []string{
		"-y",
		"-i", srcFile,
		"-i", materialFile,
		"-filter_complex",
		fmt.Sprintf("[1:v]scale=300:-1,rotate=%s:ow=rotw(iw):oh=roth(ih):c=none[wm];[0:v][wm]overlay=%s:%s",
			formatFFmpegFloat(rotate),
			formatFFmpegFloat(x),
			formatFFmpegFloat(y),
		),
		outputFile,
	}

	if err := runExecCmd(pngArgs); err != nil {
		log.Printf("❌ 合并接口失败: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"code": 500,
			"msg":  "ffmpeg合成服务器错误" + err.Error(),
		})
		return
	}

	buffer, err := os.ReadFile(outputFile)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "合成失败：未生成 PNG 文件"})
		return
	}

	log.Printf("🎉 图片合成完成，返回 Base64")

	writeJSON(w, http.StatusOK, map[string]any{
		"code": 200,
		"msg":  "合成成功",
		"data": map[string]any{
			"base64": "data:image/png;base64," + base64.StdEncoding.EncodeToString(buffer),
		},
	})
}

func copyMaterial(src, dst string) error {
	if _, err := os.Stat(src); err != nil {
		if os.IsNotExist(err) {
			return errors.New("素材图片不存在")
		}
		return fmt.Errorf("读取素材图片失败: %w", err)
	}

	input, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("打开素材图片失败: %w", err)
	}
	defer input.Close()

	output, err := os.Create(dst)
	if err != nil {
		return fmt.Errorf("创建素材图片副本失败: %w", err)
	}

	if _, err := io.Copy(output, input); err != nil {
		output.Close()
		return fmt.Errorf("复制素材图片失败: %w", err)
	}

	if err := output.Close(); err != nil {
		return fmt.Errorf("关闭素材图片副本失败: %w", err)
	}

	return nil
}
