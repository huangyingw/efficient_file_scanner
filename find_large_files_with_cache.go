package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/gob"
	"encoding/hex"
	"fmt"
	"github.com/go-redis/redis/v8"
	"github.com/karrick/godirwalk"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

var progressCounter int32 // Progress counter
var rdb *redis.Client     // Redis client
var ctx = context.Background()

// FileInfo holds file information
type FileInfo struct {
	Size    int64
	ModTime time.Time
}

// Task 定义了工作池中的任务类型
type Task func()

// NewWorkerPool 创建并返回一个工作池
func NewWorkerPool(workerCount int) (chan<- Task, *sync.WaitGroup) {
	var wg sync.WaitGroup
	taskQueue := make(chan Task)

	for i := 0; i < workerCount; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for task := range taskQueue {
				task()
			}
		}()
	}

	return taskQueue, &wg
}

var wg sync.WaitGroup
var workerPool = make(chan struct{}, 20) // Limit concurrency to 20

// Initialize Redis client
func init() {
	rdb = redis.NewClient(&redis.Options{
		Addr: "localhost:6379",
	})
	_, err := rdb.Ping(ctx).Result()
	if err != nil {
		fmt.Println("Error connecting to Redis:", err)
		os.Exit(1)
	}
}

// Generate a SHA-256 hash for the given string
func generateHash(s string) string {
	hasher := sha256.New()
	hasher.Write([]byte(s))
	return hex.EncodeToString(hasher.Sum(nil))
}

func processDirectory(path string) {
	// 处理目录的逻辑
	fmt.Printf("Processing directory: %s\n", path)
	// 可能的操作：遍历目录下的文件等
}

func processSymlink(path string) {
	// 处理软链接的逻辑
	fmt.Printf("Processing symlink: %s\n", path)
	// 可能的操作：解析软链接，获取实际文件等
}

func loadExcludePatterns(filename string) ([]string, error) {
	file, err := os.Open(filename)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	var patterns []string
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		pattern := scanner.Text()
		fmt.Printf("Loaded exclude pattern: %s\n", pattern) // 打印每个加载的模式
		patterns = append(patterns, pattern)
	}
	return patterns, scanner.Err()
}

func saveToFile(dir, filename string, sortByModTime bool) error {
	file, err := os.Create(filepath.Join(dir, filename))
	if err != nil {
		return err
	}
	defer file.Close()

	iter := rdb.Scan(ctx, 0, "*", 0).Iterator()
	var data = make(map[string]FileInfo)
	for iter.Next(ctx) {
		hashedKey := iter.Val()
		originalPath, err := rdb.Get(ctx, "path:"+hashedKey).Result()
		if err != nil {
			continue
		}
		value, err := rdb.Get(ctx, hashedKey).Bytes()
		if err != nil {
			continue
		}
		var fileInfo FileInfo
		buf := bytes.NewBuffer(value)
		dec := gob.NewDecoder(buf)
		if err := dec.Decode(&fileInfo); err == nil {
			data[originalPath] = fileInfo
		}
	}

	var keys []string
	for k := range data {
		keys = append(keys, k)
	}

	sortKeys(keys, data, sortByModTime)

	for _, k := range keys {
		relativePath, _ := filepath.Rel(dir, k)
		if sortByModTime {
			utcTimestamp := data[k].ModTime.UTC().Unix()
			fmt.Fprintf(file, "%d,\"./%s\"\n", utcTimestamp, relativePath)
		} else {
			fmt.Fprintf(file, "%d,\"./%s\"\n", data[k].Size, relativePath)
		}
	}
	return nil
}

func sortKeys(keys []string, data map[string]FileInfo, sortByModTime bool) {
	if sortByModTime {
		sort.Slice(keys, func(i, j int) bool {
			return data[keys[i]].ModTime.After(data[keys[j]].ModTime)
		})
	} else {
		sort.Slice(keys, func(i, j int) bool {
			return data[keys[i]].Size > data[keys[j]].Size
		})
	}
}

func processFile(path string, typ os.FileMode) {
	if typ.IsDir() {
		return
	}

	info, err := os.Stat(path)
	if err != nil {
		fmt.Printf("Error stating file: %s, Error: %s\n", path, err)
		return
	}

	var buf bytes.Buffer
	enc := gob.NewEncoder(&buf)
	if err := enc.Encode(FileInfo{Size: info.Size(), ModTime: info.ModTime()}); err != nil {
		fmt.Printf("Error encoding: %s, File: %s\n", err, path)
		return
	}

	// Generate hash for the file path
	hashedKey := generateHash(path)

	// 使用管道批量处理Redis命令
	pipe := rdb.Pipeline()

	// 这里我们添加命令到管道，但不立即检查错误
	pipe.Set(ctx, hashedKey, buf.Bytes(), 0)
	pipe.Set(ctx, "path:"+hashedKey, path, 0)

	if _, err = pipe.Exec(ctx); err != nil {
		fmt.Printf("Error executing pipeline for file: %s: %s\n", path, err)
		return
	}

	// Update progress counter atomically
	atomic.AddInt32(&progressCounter, 1)
}

func main() {
	if len(os.Args) < 2 {
		fmt.Println("Usage: ./find_large_files_with_cache <directory>")
		return
	}

	// Root directory to start the search
	rootDir := os.Args[1]

	// Minimum file size in bytes
	minSize := 200 // Default size is 200MB
	minSizeBytes := int64(minSize * 1024 * 1024)

	excludePatterns, err := loadExcludePatterns(filepath.Join(rootDir, "exclude_patterns.txt"))
	if err != nil {
		fmt.Println("Warning: Could not read exclude patterns:", err)
	}

	excludeRegexps := make([]*regexp.Regexp, len(excludePatterns))
	for i, pattern := range excludePatterns {
		// 将通配符模式转换为正则表达式
		regexPattern := strings.Replace(pattern, "*", ".*", -1)
		excludeRegexps[i], err = regexp.Compile(regexPattern)
		if err != nil {
			fmt.Printf("Invalid regex pattern '%s': %s\n", regexPattern, err)
			return
		}
	}

	// Start a goroutine to periodically print progress
	go func() {
		for {
			time.Sleep(1 * time.Second)
			fmt.Printf("Progress: %d files processed.\n", atomic.LoadInt32(&progressCounter))
		}
	}()

	// Use godirwalk.Walk instead of fastwalk.Walk or filepath.Walk
	// 初始化工作池
	workerCount := 20 // 可以根据需要调整工作池的大小
	taskQueue, poolWg := NewWorkerPool(workerCount)

	// 使用 godirwalk.Walk 遍历文件
	err = godirwalk.Walk(rootDir, &godirwalk.Options{
		Callback: func(osPathname string, de *godirwalk.Dirent) error {
			// 排除模式匹配
			for _, re := range excludeRegexps {
				if re.MatchString(osPathname) {
					return nil
				}
			}

			fileInfo, err := os.Lstat(osPathname)
			if err != nil {
				fmt.Printf("Error getting file info: %s\n", err)
				return err
			}

			// 检查文件大小是否满足最小阈值
			if fileInfo.Size() < minSizeBytes {
				return nil
			}

			// 将任务发送到工作池
			taskQueue <- func() {
				if fileInfo.Mode().IsDir() {
					processDirectory(osPathname)
				} else if fileInfo.Mode().IsRegular() {
					processFile(osPathname, fileInfo.Mode())
				} else if fileInfo.Mode()&os.ModeSymlink != 0 {
					processSymlink(osPathname)
				} else {
					fmt.Printf("Skipping unknown type: %s\n", osPathname)
				}
			}

			return nil
		},
		Unsorted: true,
	})

	// 关闭任务队列，并等待所有任务完成
	close(taskQueue)
	poolWg.Wait()
	fmt.Printf("Final progress: %d files processed.\n", atomic.LoadInt32(&progressCounter))

	// 文件处理完成后的保存操作
	if err := saveToFile(rootDir, "fav.log", false); err != nil {
		fmt.Printf("Error saving to fav.log: %s\n", err)
	} else {
		fmt.Printf("Saved data to %s\n", filepath.Join(rootDir, "fav.log"))
	}

	if err := saveToFile(rootDir, "fav.log.sort", true); err != nil {
		fmt.Printf("Error saving to fav.log.sort: %s\n", err)
	} else {
		fmt.Printf("Saved sorted data to %s\n", filepath.Join(rootDir, "fav.log.sort"))
	}
}
