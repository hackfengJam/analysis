package main

import (
	"bufio"
	"crypto/md5"
	"encoding/hex"
	"flag"
	"github.com/mediocregopher/radix.v2/pool"
	"github.com/mgutz/str"
	"github.com/sirupsen/logrus"
	"io"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

const HANDLE_DIG = " /dig?"
const HANDLE_MOVIE = "/movie/"
const HANDLE_LIST = "/list/"
const HANDLE_HTML = ".html"

type cmdParams struct {
	logFilePath string
	routineNum  int
}
type digData struct {
	time  string
	url   string
	refer string
	ua    string
}
type urlData struct {
	data  digData
	uid   string
	unode urlNode
}
type urlNode struct {
	unType string // 详情页 或者 列表页 或者 首页
	unRid  int    // Resource ID  资源ID
	unUrl  string // 当前页面的 url
	unTime string // 当前访问页面的时间
}
type storageBlock struct {
	counterType  string
	storageModel string
	unode        urlNode
}

var log = logrus.New()
//var redisCli redis.Client

func init() {
	log.Out = os.Stdout
	log.SetLevel(logrus.DebugLevel)
	/*	redisCli, err := redis.Dial("tcp", "localhost:6379")
		if err != nil {
			log.Fatalln("Redis connect failed")
		} else {
			defer redisCli.Close()
		}*/
}

func main() {
	// 获取参数
	logFilePath := flag.String("logFilePath", "/User/hack/dig.log", "log file path")
	routineNum := flag.Int("routineNum", 5, "consumer number by goroutine")
	l := flag.String("l", "tmp/log", "this programme runtime log target file path")
	flag.Parse()
	params := cmdParams{*logFilePath, *routineNum}

	// 打日志
	logFd, err := os.OpenFile(*l, os.O_CREATE|os.O_WRONLY, 0644)
	if err == nil {
		log.Out = logFd
		defer logFd.Close()
	}
	log.Infoln("Exec start.")
	log.Infof("Params: logFilePath=%s, routineNums=%d", params.logFilePath, params.routineNum)

	// 初始化一些 channel，用于数据传递
	var logChannel = make(chan string, 3*params.routineNum)
	var pvChannel = make(chan urlData, params.routineNum)
	var uvChannel = make(chan urlData, params.routineNum)
	var storageChannel = make(chan storageBlock, params.routineNum)

	// Redis Pool
	redisPool, err := pool.New("tcp", "localhost:6379", 2*params.routineNum)
	if err != nil {
		log.Fatalln("Redis pool created failed.")
		panic(err)
	} else {
		go func() {
			for {
				redisPool.Cmd("PING")
				time.Sleep(3 * time.Second)
			}
		}()
	}

	// 日志消费者
	go readFileLineByLine(params, logChannel)

	// 创建一组日志处理
	for i := 0; i < params.routineNum; i++ {
		go logConsumer(logChannel, pvChannel, uvChannel)
	}

	// 创建PV UV 统计器
	go pvCounter(pvChannel, storageChannel)
	go uvCounter(uvChannel, storageChannel, redisPool)
	// 可扩展的 xxxCounter

	// 创建存储器
	go dataStorage(storageChannel, redisPool)

	time.Sleep(1000 * time.Second)
}

// HBase 列式存储，劣势：列簇 需要声明清楚（列变来变去不方便）
func dataStorage(storageChannel chan storageBlock, redisPool *pool.Pool) {
	for block := range storageChannel {
		prefix := block.counterType + "_"

		// 10000 * N * M
		// 逐层添加，加洋葱皮的过程
		// 维度： 天-小时-分钟
		// 层级： 定级-大分类-小分类-终极页面
		// 存储模型： Redis SortedSet
		setKeys := []string{
			prefix + "day_" + getTime(block.unode.unTime, "day"),
			prefix + "hour_" + getTime(block.unode.unTime, "hour"),
			prefix + "min_" + getTime(block.unode.unTime, "min"),
			prefix + block.unode.unType + "_day_" + getTime(block.unode.unTime, "day"),
			prefix + block.unode.unType + "_hour_" + getTime(block.unode.unTime, "hour"),
			prefix + block.unode.unType + "_min_" + getTime(block.unode.unTime, "min"),
		}

		rowId := block.unode.unRid

		for _, key := range setKeys {
			ret, err := redisPool.Cmd(block.storageModel, key, 1, rowId).Int()
			if err != nil || ret <= 0 {
				log.Error("DataStorage redis storage error.", block.storageModel, key, rowId)
			}
		}
	}
}

func uvCounter(uvChannel chan urlData, storageChannel chan storageBlock, redisPool *pool.Pool) {
	for data := range uvChannel {
		// HyperLoglog redis 去重
		hyperLogLogKey := "uv_hpll_" + getTime(data.data.time, "day")
		ret, err := redisPool.Cmd("PFADD", hyperLogLogKey, data.uid, "EX", 86400).Int()
		if err != nil {
			log.Warning("uvCounter check redis hyperloglog failed, ", err)
		}
		if ret != 1 {
			continue
		}
		sItem := storageBlock{"uv", "ZINCRBY", data.unode}
		storageChannel <- sItem
	}
}

func pvCounter(pvChannel chan urlData, storageChannel chan storageBlock) {
	for data := range pvChannel {
		sItem := storageBlock{"pv", "ZINCRBY", data.unode}
		storageChannel <- sItem
	}
}

func logConsumer(logChannel chan string, pvChannel, uvChannel chan urlData) error {
	for logStr := range logChannel {
		// 切割日志字符串，扣出打点上报的数据
		data := cutLogFetchData(logStr)

		// uid
		// 说明： 课程中模拟生成uid,   md5(refer+ua)
		hasher := md5.New()
		hasher.Write([]byte(data.refer + data.ua))
		uid := hex.EncodeToString(hasher.Sum(nil))

		// 很多解析的工作都可放在这里完成
		// ...
		// ...
		uData := urlData{
			data,
			uid,
			formatUrl(data.url, data.time),
		}

		//log.Infoln(uData)

		pvChannel <- uData
		uvChannel <- uData
		//xxChannel <- uData
	}
	return nil
}
func cutLogFetchData(logStr string) digData {
	logStr = strings.TrimSpace(logStr)
	pos1 := str.IndexOf(logStr, HANDLE_DIG, 0)
	if pos1 == -1 {
		return digData{}
	}
	pos1 += len(HANDLE_DIG)
	pos2 := str.IndexOf(logStr, " HTTP/", pos1)
	d := str.Substr(logStr, pos1, pos2-pos1)

	urlInfo, err := url.Parse("http://localhost/?" + d)
	if err != nil {
		return digData{}
	}
	data := urlInfo.Query()
	return digData{
		data.Get("time"),
		data.Get("refer"),
		data.Get("url"),
		data.Get("ua"),
	}

}

func readFileLineByLine(params cmdParams, logChannel chan string) error {
	fd, err := os.Open(params.logFilePath)
	if err != nil {
		log.Warning("ReadFileLineByLine can't open file:%s", params.logFilePath)
		return err
	}
	defer fd.Close()

	count := 0
	bufferRead := bufio.NewReader(fd)
	for {
		line, err := bufferRead.ReadString('\n')
		logChannel <- line
		count++

		if count%(1000*params.routineNum) == 0 {
			log.Infof("ReadFileLineByLine line:%d", count)
		}
		if err != nil {
			if err == io.EOF {
				time.Sleep(3 * time.Second)
				log.Infof("ReadFileLineByLine wait, readLine: %d", count)
			} else {
				log.Infof("ReadFileLineByLine read log error")
			}
		}
	}
	return nil
}

func formatUrl(url, t string) urlNode {
	// 一定从量大的着手， 详情页>列表≥首页
	pos1 := str.IndexOf(url, HANDLE_MOVIE, 0)
	if pos1 != -1 {
		pos1 += len(HANDLE_MOVIE)
		pos2 := str.IndexOf(url, HANDLE_HTML, 0)
		idStr := str.Substr(url, pos1, pos2-pos1)
		id, _ := strconv.Atoi(idStr)
		return urlNode{"movie", id, url, t}
	} else {
		pos1 = str.IndexOf(url, HANDLE_LIST, 0)
		if pos1 != -1 {
			pos1 += len(HANDLE_LIST)
			pos2 := str.IndexOf(url, HANDLE_HTML, 0)
			idStr := str.Substr(url, pos1, pos2-pos1)
			id, _ := strconv.Atoi(idStr)
			return urlNode{"list", id, url, t}
		} else {
			return urlNode{"home", 1, url, t}
		} // 如果页面多种，不断在此扩展
	}
}

func getTime(logTime, timeType string) string {
	var item string
	switch timeType {
	case "day":
		item = "2006-01-02"
		//fallthrough   //必须是最后一个语句
		break // 默认
	case "hour":
		item = "2006-01-02 15"
		break
	case "min":
		item = "2006-01-02 15:04"
		break
	}
	t, _ := time.Parse(item, time.Now().Format(item))
	return strconv.FormatInt(t.Unix(), 10)
}
