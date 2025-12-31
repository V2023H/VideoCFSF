package main

import (
	"bytes"
	"crypto/md5"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"github.com/gorilla/websocket"
	"io"
	"log"
	_ "modernc.org/sqlite"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"os/signal"
	"syscall"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// --- 数据结构定义 ---
var run_dir string
var scr_dir string
var new_dir string
var db *sql.DB
var startstatus bool=false
var scaninfo ScanRequest
var msg_Total int=0
var msg_Current int=0
var msg_Content string=""

// FileRecord 对应一行数据
type FileRecord struct {
	ID     int
	Path   string
	Status string
	Code   string
	MD5    string
	mode   string
	date   string
	dir    string
}


// ScanRequest 接收前端发送的扫描请求
type ScanRequest struct {
	Directory string `json:"directory"`
	ScanMode  string `json:"scanMode"` // "quick" 或 "full"
	ScanType  string `json:"scanType"` // "skip1" 或 "skip2"
}

// LogMessage 发送给前端的实时日志信息
type LogMessage struct {
	Type    string `json:"type"` // "log", "result", "progress"
	Content string `json:"content"`
	Path    string `json:"path,omitempty"`    // 仅用于 result
	Status  string `json:"status,omitempty"`  // "ok" 或 "broken"
	Current int    `json:"current,omitempty"` // 仅用于 progress
	Total   int    `json:"total,omitempty"`   // 仅用于 progress
}

// --- WebSocket 和 HTTP 设置 ---

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool {
		return true // 允许跨域连接
	},
}

var conn *websocket.Conn // 存储当前的 WebSocket 连接
var mutex sync.Mutex     // 保护 conn 变量和扫描过程

// --- 核心功能：ffprobe 检查 ---

// isVideoBroken 检查视频文件是否损坏
func isVideoBroken(filePath string, scanMode string, directory string) bool {

	//ffmpeg -i "/data/v/3388236.avi" -show_entries format=duration -v quiet -of csv="p=0"
	//ffmpeg -i 中断1.mp4 -ss 238.5 -max_error_rate 0 -vframes 1 -r 1 -ac 2 -ab 128k  -s 90x80 -y -f mjpeg 中断1.jpg
	// Full Mode: 尝试生成一帧缩略图 (更彻底，但更慢)

	// 准备用于捕获输出的缓冲区
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	var iscopy_int int = 0
	cmd := &exec.Cmd{}
	fullCommand := ""
	if scanMode == "full" {
		// 使用 ffmpeg 获取视频时长
		args := []string{
			"-i", filePath,
			"-show_entries", "format=duration",
			"-v", "quiet",
			"-of", "csv=p=0",
		}
		cmd = exec.Command("ffprobe", args...)
		cmd.Stdout = &stdout
		cmd.Run()
		str_time := stdout.String()
		str_time = strings.TrimSpace(str_time)
		num_temp, err := strconv.ParseFloat(str_time, 64)
		if err != nil {
			log.Println("无法解析视频时长:", err)
			iscopy_int = 10000
		}
		if iscopy_int <= 0 {
			// 时间段数组
			str_time_arr := []string{fmt.Sprintf("%.1f", 0.5), fmt.Sprintf("%.1f", num_temp/2), fmt.Sprintf("%.1f", num_temp-0.5)}
			for index, str_time := range str_time_arr {
				if num_temp <= 0.5 {
					continue
				}
				// 尝试生成一个 1x1 像素的缩略图到 /dev/null 或 NUL
				// 这比只检查流信息更能发现某些类型的损坏
				args = []string{
					"-ss", str_time, // 尝试从视频的第1秒开始
					"-i", filePath,
					"-v", "error",
					"-max_error_rate", "0",
					"-vframes", "1",
					"-r", "1",
					"-strict", "unofficial",
					"-ac", "2",
					"-ab", "128k",
					"-vf", "scale=-2:-2",
					"-y", // 覆盖输出文件 (尽管我们输出到 null)
					"-f", "mjpeg",
					run_dir + "cache.jpg",
				}
				cmd = exec.Command("ffmpeg", args...)
				//fullCommand = strings.Join(cmd.Args, " ")
				//sendMessage(LogMessage{Type: "log", Content: "命令:" + fullCommand})
				// 尝试执行命令
				stderr.Reset()
				cmd.Stderr = &stderr
				err := cmd.Run()
				//stderrContent := stderr.String() + strings.Join(cmd.Args, " ")
				//os.WriteFile(filePath+strconv.Itoa(index)+".txt", []byte(stderrContent), 0644)
				// 如果命令执行失败 (非零退出码)，则认为文件损坏
				if err != nil {
					int_time := 0
					if index == 1 {
						int_time = 1
					} else if index == 2 {
						int_time = 10
					} else {
						int_time = 100
					}
					iscopy_int = iscopy_int + int_time
				}
			}
		}
		// 尝试使用 FFmpeg 识别损坏点击（黑盒检测）
		if iscopy_int <= 0 {
			stderr.Reset()
			//使用 FFmpeg 识别损坏点（黑盒检测）
			args = []string{
				"-v", "error",
				"-i", filePath,
				"-f", "null",
				"-",
			}

			cmd = exec.Command("ffmpeg", args...)
			fullCommand = strings.Join(cmd.Args, " ")
			cmd.Stderr = &stderr
			cmd.Run()
			stderrContent := stderr.String()
			//os.WriteFile(filePath+"3.txt", []byte(stderrContent), 0644)
			keywords := []string{"damaged", "Error at MB", "slice end not reached"}
			for _, word := range keywords {
				if strings.Contains(stderrContent, word) {
					iscopy_int = iscopy_int + 1000
					break
				}
			}
		}
	} else {
		// 使用 ffprobe 检查视频流信息。
		// 损坏的视频通常会导致 ffprobe 无法成功解析流信息并返回非零退出码。
		// -v error: 只输出错误信息
		// -select_streams v: 只检查视频流
		// -show_entries stream=codec_name: 显示视频编解码器名称
		// -of default=noprint_wrappers=1: 只输出核心内容

		// Quick Mode: 仅检查流信息 (速度较快)
		args := []string{
			"-v", "error",
			"-select_streams", "v",
			"-show_entries", "stream=codec_name",
			"-of", "default=noprint_wrappers=1",
			filePath,
		}
		cmd = exec.Command("ffprobe", args...)
		fullCommand = strings.Join(cmd.Args, " ")
		sendMessage(LogMessage{Type: "log", Content: "命令:" + fullCommand})

		// 尝试执行命令
		cmd.Run()
	}
	// 处理命令输出和错误
	// sendMessage(LogMessage{Type: "log", Content: "错误:" + stderr.String()})
	if iscopy_int >= 1000 {
		new_dir = strings.ReplaceAll(filePath, scr_dir, scr_dir+"_碎片损坏文件备份")
	} else if iscopy_int >= 1 {
		new_dir = strings.ReplaceAll(filePath, scr_dir, scr_dir+"_严重损坏文件备份")
	} else {
		new_dir = strings.ReplaceAll(filePath, scr_dir, scr_dir+"_正常文件")
	}
	// new_dir = strings.ReplaceAll(new_dir, ":", "")
	// new_dir = strings.ReplaceAll(new_dir, "//", "/")
	// new_dir = "E:\\" + strings.ReplaceAll(new_dir, "\\\\", "\\")
	if iscopy_int != 0 {
		targetDir := filepath.Dir(new_dir)
		if err := os.MkdirAll(targetDir, 0755); err != nil {
			fmt.Println("创建目标目录失败:", targetDir)
		}

		if err := os.Rename(filePath, new_dir); err != nil {
			fmt.Println("移动文件失败:", filePath, "->", new_dir)
		}
		fmt.Println("\n移动文件:", filePath+" -> "+new_dir)
	}

	now := time.Now()
	layout := "2006-01-02 15:04:05"
	formattedTime := now.Format(layout)

	md5Hash, _ := getFileMD5(filePath)
	isBroken_string := ""
	isBroken_bool := false
	if iscopy_int >= 1 {
		isBroken_string = "损坏"
		isBroken_bool = true
	} else {
		isBroken_string = "正常"
		isBroken_bool = false
	}
	// 插入数据
	insertFile(db, FileRecord{
		Path:   filePath,
		Status: isBroken_string,
		Code:   strconv.Itoa(iscopy_int),
		MD5:    md5Hash,
		mode:   scanMode,
		date:   formattedTime,
		dir:    directory,
	})
	return isBroken_bool // 损坏
}

func getFileMD5(filePath string) (string, error) {
	// 1. 打开文件
	file, err := os.Open(filePath)
	if err != nil {
		return "", err
	}
	defer file.Close()

	// 2. 创建一个 MD5 哈希句柄
	hash := md5.New()

	// 3. 使用 io.Copy 将文件内容流式传输到哈希计算器中
	// 这不会把整个文件加载到内存，适合处理 GB 级别的大文件
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}

	// 4. 计算最终哈希值并转换为 16 进制字符串
	hashInBytes := hash.Sum(nil)
	return hex.EncodeToString(hashInBytes), nil
}

// --- 扫描和通信逻辑 ---

// sendMessage 封装了发送 WebSocket 消息的逻辑
func sendMessage(msg LogMessage) {
	mutex.Lock()
	defer mutex.Unlock()
	if conn != nil {
		if err := conn.WriteJSON(msg); err != nil {
			log.Println("WebSocket 写入错误:", err)
			// 如果发生写入错误，可以考虑关闭连接
			// conn.Close()
			// conn = nil
		}
	}
}

// startScan 开始文件扫描过程 (在 goroutine 中运行)
func startScan(req ScanRequest) {
	if req.ScanMode == "printdb" {
		fmt.Println("打印数据库记录中...")
		// 打印数据库记录
		rows, err := db.Query("SELECT * FROM files")
		if err != nil {
			log.Println("查询数据库记录失败:", err)
			sendMessage(LogMessage{Type: "log", Content: "错误: 查询数据库记录失败"})
			return
		}
		defer rows.Close()
		var subpath string
		for rows.Next() {
			var rec FileRecord
			if err := rows.Scan(&rec.ID, &rec.Path, &rec.Status, &rec.Code, &rec.MD5, &rec.mode, &rec.date, &rec.dir); err != nil {
				log.Println("扫描行失败:", err)
				continue
			}
			var result string
			if rec.Status == "正常" {
				result = "ok"
			} else {
				result = "broken"
			}
			//根据系统分割目录途径文本
			if runtime.GOOS == "windows" {
				dirnamearr := strings.Split(FixDirPath(req.Directory), "\\")
				//取最后一个目录名
				dirname := dirnamearr[len(dirnamearr)-2]
				if strings.HasPrefix(rec.Path, "/") {
					dirnamearr = strings.Split(rec.Path, "/"+dirname+"/")
				} else {
					dirnamearr = strings.Split(rec.Path, "\\"+dirname+"\\")
				}
				subpath = FixDirPath(req.Directory) + dirnamearr[len(dirnamearr)-1]
				if len(dirnamearr) == 2 {
					subpath = FixDirPath(req.Directory) + dirnamearr[len(dirnamearr)-1]
					subpath = strings.ReplaceAll(subpath, "/", "\\")
				}
			} else {
				dirnamearr := strings.Split(FixDirPath(req.Directory), "/")
				//取最后一个目录名
				dirname := dirnamearr[len(dirnamearr)-2]
				if strings.HasPrefix(rec.Path, "/") {
					dirnamearr = strings.Split(rec.Path, "/"+dirname+"/")
				} else {
					dirnamearr = strings.Split(rec.Path, "\\"+dirname+"\\")
				}
				if len(dirnamearr) == 2 {
					subpath = FixDirPath(req.Directory) + dirnamearr[len(dirnamearr)-1]
					subpath = strings.ReplaceAll(subpath, "\\", "/")
				}
			}
			_, err := os.Stat(subpath)
			if err == nil {
				//fmt.Println(subpath + " - 文件存在")
			} else if os.IsNotExist(err) {
				//fmt.Println(subpath + " - 文件不存在")
			} else {
				//fmt.Println(subpath + " - 文件不存在")
			}
			subpath = ""
			// 发送结果
			sendMessage(LogMessage{
				Type:   "result",
				Path:   fmt.Sprintf("ID: %d, 路径: %s", rec.ID, rec.Path),
				Status: result,
			})
		}
		fmt.Println("打印数据库记录完毕...")
		sendMessage(LogMessage{Type: "log", Content: "打印数据库记录完毕"})
		return
	}
	if req.ScanMode == "cleardb" {
		os.Remove("static/index.html")
		copyFile("static/index.htm", "static/index.html")
		fmt.Println("清除数据库记录中...")
		// 清除数据库记录
		_, err := db.Exec("DELETE FROM files")
		if err != nil {
			log.Println("清除数据库记录失败:", err)
			sendMessage(LogMessage{Type: "log", Content: "错误: 删除数据库记录失败"})
			return
		}
		log.Println("数据库已清空")
		sendMessage(LogMessage{Type: "log", Content: "数据库已清空"})
		return
	}
	mode := ""
	if req.ScanMode == "movebroken" || req.ScanMode == "movenormal" {
		if req.ScanMode == "movenormal" {
			mode = "正常"
		} else {
			mode = "损坏"
		}
		fmt.Println("移动" + mode + "文件中...")
		// 移动文件中
		rows, err := db.Query("SELECT path FROM files WHERE status = '" + mode + "'")
		if err != nil {
			log.Println("查询"+mode+"文件失败:", err)
			sendMessage(LogMessage{Type: "log", Content: "错误: 查询" + mode + "文件失败"})
			return
		}
		defer rows.Close()

		for rows.Next() {
			var filePath string
			if err := rows.Scan(&filePath); err != nil {
				log.Println("扫描行失败:", err)
				continue
			}
			new_dir = FixDirPath(req.Directory) + mode + strings.ReplaceAll(filePath, ":", "")
			new_dir = strings.ReplaceAll(new_dir, "//", "/")
			new_dir = strings.ReplaceAll(new_dir, "\\\\", "\\")
			targetDir := filepath.Dir(new_dir)
			if err := os.MkdirAll(targetDir, 0755); err != nil {
				fmt.Println("创建目标目录失败:", targetDir)
			}

			if err := os.Rename(filePath, new_dir); err != nil {
				fmt.Println("移动文件失败:", filePath, "->", new_dir)
			} else {
				fmt.Println("\n移动文件:", filePath+" -> "+new_dir)
			}
		}
		fmt.Println("移动" + mode + "文件完毕...")
		sendMessage(LogMessage{Type: "log", Content: "移动" + mode + "文件完毕"})
		return
	}

	if req.ScanMode == "full" || req.ScanMode == "quick" {
		//开始前...
		startstatus = true
		scaninfo=req
		sendMessage(LogMessage{Type: "log", Content: "开始扫描..."})

		// 视频文件扩展名列表
		videoExts := map[string]bool{
			".mp4": true, ".mov": true, ".avi": true, ".mkv": true,
			".wmv": true, ".flv": true, ".webm": true,
		}

		var files []string

		// 遍历目录查找视频文件
		err := filepath.Walk(FixDirPath(req.Directory), func(path string, info os.FileInfo, err error) error {
			scr_dir = filepath.FromSlash(req.Directory)

			if err != nil {
				// 忽略无法访问的文件/目录，记录日志并继续
				sendMessage(LogMessage{Type: "log", Content: fmt.Sprintf("警告: 无法访问 %s: %v", path, err)})
				return filepath.SkipDir // 跳过整个目录
			}

			if !info.IsDir() {
				ext := filepath.Ext(path)
				if videoExts[ext] {
					files = append(files, path)
				}
			}
			return nil
		})

		if err != nil {
			sendMessage(LogMessage{Type: "log", Content: fmt.Sprintf("❌ 目录遍历失败: %v", err)})
			sendMessage(LogMessage{Type: "progress", Current: 0, Total: 0}) // 重置进度条
			return
		}

		totalFiles := len(files)
		sendMessage(LogMessage{Type: "log", Content: fmt.Sprintf("找到 %d 个视频文件，开始 %s 模式检查...", totalFiles, req.ScanMode)})
		// --- 检查循环 ---
		for i, file := range files {
			
			time.Sleep(10 * time.Millisecond) // 避免 CPU 占用过高

			//查找文件路径是否在数据库中存在，存在则跳过
			var isfind bool = false
			var rec FileRecord
			err := db.QueryRow("SELECT id, Status, path FROM files WHERE path = ?", file).Scan(&rec.ID, &rec.Status, &rec.Path)
			if err != nil {
				if err == sql.ErrNoRows {
				} else {
					log.Fatal("数据库连接失败或 SQL 语法错误:", err)
				}
			} else {
				isfind = true
			}
			if isfind == true {
				if req.ScanType == "skip1" || (req.ScanType == "skip2" && rec.Status == "正常") {
					sendMessage(LogMessage{Type: "log", Content: fmt.Sprintf("跳过已检查文件: %s", file)})
					// 发送进度更新
					sendMessage(LogMessage{
						Type:    "progress",
						Current: i + 1,
						Total:   totalFiles,
						Content: fmt.Sprintf("跳过已检查文件: %s", filepath.Base(file)),
					})
					msg_Current=i+1
					msg_Total=totalFiles
					msg_Content=fmt.Sprintf("跳过已检查文件: %s", filepath.Base(file))
					// 发送结果
					sendMessage(LogMessage{
						Type:   "result",
						Path:   file,
						Status: "skipped",
					})
					continue
				}
				if req.ScanType == "skip2" {
					deleteFile(db, int64(rec.ID))
				}
			}
			// 1. 发送进度
			sendMessage(LogMessage{
				Type:    "progress",
				Current: i + 1,
				Total:   totalFiles,
				Content: fmt.Sprintf("正在检查: %s", filepath.Base(file)),
			})
			msg_Current=i+1
			msg_Total=totalFiles
			msg_Content=fmt.Sprintf("正在检查: %s", filepath.Base(file))

			// 2. 检查文件
			isBroken := isVideoBroken(file, req.ScanMode, req.Directory)

			status := "ok"
			if isBroken {
				status = "broken"
				sendMessage(LogMessage{Type: "log", Content: fmt.Sprintf("❗ 发现损坏文件: %s", file)})
			}

			// 3. 发送结果
			sendMessage(LogMessage{
				Type:   "result",
				Path:   file,
				Status: status,
			})
		}

		sendMessage(LogMessage{Type: "log", Content: "✅ 扫描完成。"})
		// 最终进度更新
		sendMessage(LogMessage{Type: "progress", Current: totalFiles, Total: totalFiles})
		startstatus = false
		// 结束后
		os.Remove("cache.jpg")
	}
}

// --- HTTP 处理器 ---

// wsHandler 处理 WebSocket 连接
func wsHandler(w http.ResponseWriter, r *http.Request) {
	var err error

	// 升级 HTTP 连接到 WebSocket
	conn, err = upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println("WebSocket 升级失败:", err)
		return
	}
	defer conn.Close()

	if startstatus {
		log.Println("重新打开页面，加载扫描进度...")
		data, err := os.ReadFile(filepath.Join("static", "index.htm"))
		if err != nil {
			fmt.Println("未找到源文件")
			return
		}
		htmlContent := strings.ReplaceAll(string(data), "{{dir_path}}", scaninfo.Directory)
		htmlContent = strings.ReplaceAll(string(htmlContent), "{{scanMode}}", scaninfo.ScanMode)
		htmlContent = strings.ReplaceAll(string(htmlContent), "{{scanType}}", scaninfo.ScanType)
		htmlContent = strings.ReplaceAll(string(htmlContent), "{{status}}", "startstatus")
		err = os.WriteFile("static/index.html", []byte(htmlContent), 0644)
		if err != nil { 
			fmt.Println("文件html写入失败")
			return
		}
		log.Println("WebSocket 继续建立连接。")
		sendMessage(LogMessage{Type: "progress", Current: msg_Current, Total: msg_Total,Content: msg_Content}) // 继续进度条
	}else{
		log.Println("WebSocket 连接建立成功。")
	}

	// 保持连接，循环读取来自前端的消息
	for {
		_, message, err := conn.ReadMessage()
		if err != nil {
			// 连接关闭或读取错误
			log.Println("WebSocket 读取错误或连接关闭:", err)
			mutex.Lock()
			conn = nil // 清理全局连接
			mutex.Unlock()
			break
		}

		// 解析请求
		var req ScanRequest
		if err := json.Unmarshal(message, &req); err != nil {
			log.Println("解析请求 JSON 失败:", err)
			continue
		}

		log.Printf("收到扫描请求: %+v", req)

		// 启动扫描过程，在新 goroutine 中运行，不阻塞主 WebSocket 循环
		go startScan(req)
	}
}

func main() {
	var err error
	run_dir, err = os.Getwd()
	if err != nil {
		return
	}
	run_dir = filepath.Clean(run_dir) + string(os.PathSeparator)

	log.Printf("运行目录: %s", run_dir)

	// 打开数据库（不存在会自动创建）
	db, err = sql.Open("sqlite", filepath.Join(run_dir, "data.db"))
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	//  创建表
	createTable(db)
	//listFiles(db)
	//replacedbstr(db,"dir","F:\\","/mnt/dav/")

	// 检查html文件 
	os.Remove("static/index.html")
	_, err = os.Stat("static/index.html")
	if err != nil { copyFile("static/index.htm", "static/index.html") }

	// 创建静态文件服务器
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// 直接从嵌入文件系统读取 index.html
		data, err := os.ReadFile(filepath.Join("static", "index.html"))
		if err != nil {
			http.Error(w, "页面未找到", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		htmlContent := ""
		if runtime.GOOS == "windows" {
			htmlContent = strings.ReplaceAll(string(data), "{{dir_path}}", "d:\\1\\2")
		} else {
			htmlContent = strings.ReplaceAll(string(data), "{{dir_path}}", "/home/1/2")
		}
		htmlContent = strings.ReplaceAll(string(htmlContent), "{{scanMode}}", "full")
		htmlContent = strings.ReplaceAll(string(htmlContent), "{{scanType}}", "skip1")
		w.Write([]byte(htmlContent))
	})
	http.HandleFunc("/ws", wsHandler)

	port := "8080"
	log.Printf("Web 服务启动在 http://localhost:%s", port)
	openBrowser("http://localhost:" + port)

	// 创建信号监听通道
	sigs := make(chan os.Signal, 1)
	// 监听 Ctrl+C (SIGINT) 和 终止信号 (SIGTERM)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		log.Fatal(http.ListenAndServe(":"+port, nil))
	}()

	// 阻塞等待退出信号
	sig := <-sigs
	log.Println("收到退出信号:", sig)
	log.Println("准备退出程序...")
	os.Remove("cache.jpg")

	// 在这里执行具体的清理逻辑
	log.Println("程序退出完成")
}

// OpenBrowser 使用系统的默认浏览器打开指定的 URL
func openBrowser(url string) {
	var err error
	switch runtime.GOOS {
	case "linux":
		err = exec.Command("xdg-open", url).Start()
	case "windows":
		err = exec.Command("cmd", "/c", "start", url).Start()
	case "darwin":
		err = exec.Command("open", url).Start()
	}
	if err != nil {
		log.Printf("无法自动打开浏览器，请手动访问: %s", url)
	}
}

// ---------- 数据库操作 ----------

func createTable(db *sql.DB) {
	sqlStmt := `
	CREATE TABLE IF NOT EXISTS files (
		id     INTEGER PRIMARY KEY AUTOINCREMENT,
		path   TEXT NOT NULL,
		status TEXT NOT NULL,
		code   TEXT NOT NULL,
		md5    TEXT NOT NULL,
		mode   TEXT NOT NULL,
		date   TEXT NOT NULL,
		dir    TEXT NOT NULL
	);
	`
	_, err := db.Exec(sqlStmt)
	if err != nil {
		log.Fatal("创建表失败:", err)
	}
}

func insertFile(db *sql.DB, f FileRecord) int64 {
	result, err := db.Exec(
		"INSERT INTO files (path, status, code, md5, mode, date, dir) VALUES (?, ?, ?, ?, ?, ?, ?)",
		f.Path, f.Status, f.Code, f.MD5, f.mode, f.date, f.dir,
	)
	if err != nil {
		log.Fatal("插入失败:", err)
	}

	id, _ := result.LastInsertId()
	return id
}

func listFiles(db *sql.DB) {
	rows, err := db.Query("SELECT id, path, status, code, md5, mode, date, dir FROM files")
	if err != nil {
		log.Fatal("查询失败:", err)
	}
	defer rows.Close()

	for rows.Next() {
		var f FileRecord
		err := rows.Scan(&f.ID, &f.Path, &f.Status, &f.Code, &f.MD5, &f.mode, &f.date, &f.dir)
		if err != nil {
			log.Fatal(err)
		}
		fmt.Printf("%+v\n", f)
	}
}

func replacedbstr(db *sql.DB, path, oldStr, newStr string) (int64, error) {
	result, err := db.Exec(`
		UPDATE files
		SET `+path+` = REPLACE(`+path+`, ?, ?)
		WHERE `+path+` LIKE '%' || ? || '%'
	`, oldStr, newStr, oldStr)
	if err != nil {
		return 0, err
	}

	return result.RowsAffected()
}
//复制文件
func copyFile(src, dst string) error {
	sourceFile, err := os.Open(src)
	if err != nil {
		return err
	}
	defer sourceFile.Close()

	// 创建目标文件
	destFile, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer destFile.Close()

	_, err = io.Copy(destFile, sourceFile)
	return err
}
func deleteFile(db *sql.DB, id int64) {
	_, err := db.Exec("DELETE FROM files WHERE id = ?", id)
	if err != nil {
		log.Fatal("删除失败:", err)
	}
}

// FixDirPath 确保目录格式符合当前操作系统规范，且末尾一定有斜杠
func FixDirPath(path string) string {
	if path == "" {
		return ""
	}

	// 1. 统一转换：将所有斜杠转为当前系统的分隔符 (\ 或 /)
	// FromSlash 在 Windows 上把 / 转为 \，在 Linux 上不操作
	p := filepath.FromSlash(path)

	// 2. 清理路径：去除多余的斜杠、处理 . 和 ..
	p = filepath.Clean(p)

	// 3. 补齐末尾：获取当前系统的分隔符
	sep := string(os.PathSeparator)
	if !strings.HasSuffix(p, sep) {
		p += sep
	}

	return p
}

// CopyFileByPath 根据源路径和目标路径复制文件
func CopyFileByPath(srcPath, dstPath string) error {
	// 1. 打开源文件路径
	src, err := os.Open(srcPath)
	if err != nil {
		return err
	}
	defer src.Close()

	// 2. 确保目标目录存在（防止路径中包含不存在的文件夹）
	err = os.MkdirAll(filepath.Dir(dstPath), 0755)
	if err != nil {
		return err
	}

	// 3. 创建目标文件路径
	dst, err := os.Create(dstPath)
	if err != nil {
		return err
	}
	defer dst.Close()

	// 4. 执行复制（将数据从 src 流向 dst）
	_, err = io.Copy(dst, src)
	return err
}
