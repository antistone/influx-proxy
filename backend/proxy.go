package backend

import (
    "bytes"
    "encoding/json"
    "fmt"
    "github.com/chengshiwen/influx-proxy/util"
    "log"
    "net/http"
    "os"
    "regexp"
    "stathat.com/c/consistent"
    "strings"
    "sync"
    "time"
)

// Proxy 集群
type Proxy struct {
    Circles                []*Circle                    `json:"circles"`     // 集群列表
    ListenAddr             string                       `json:"listen_addr"` // 服务监听地址
    DataDir                string                       `json:"data_dir"`    // 缓存文件目录
    DbList                 []string                     `json:"db_list"`     // 数据库列表
    DbMap                  map[string]bool              `json:"db_map"`      // 数据库字典
    VNodeSize              int                          `json:"vnode_size"`  // 虚拟节点数
    FlushSize              int                          `json:"flush_size"`  // 实例的缓冲区大小
    FlushTime              time.Duration                `json:"flush_time"`  // 实例的缓冲清空时间
    MigrateMaxCpus         int                          `json:"migrate_max_cpus"` // 迁移时可用cpu数
    Username               string                       `json:"username"`         // proxy用户
    Password               string                       `json:"password"`         // proxy密码
    HTTPSEnabled           bool                         `json:"https_enabled"`    // https开关
    HTTPSCert              string                       `json:"https_cert"`       // https证书
    HTTPSKey               string                       `json:"https_key"`        // https密钥
    ForbiddenQuery         []*regexp.Regexp             `json:"forbidden_query"`
    ObligatedQuery         []*regexp.Regexp             `json:"obligated_query"`
    ClusteredQuery         []*regexp.Regexp             `json:"clustered_query"`
    BackendRebalanceStatus []map[string]*MigrationInfo  `json:"backend_re_balance_status"`
    BackendRecoveryStatus  []map[string]*MigrationInfo  `json:"backend_recovery_status"`
    BackendResyncStatus    []map[string]*MigrationInfo  `json:"backend_resync_status"`
}

// LineData 数据传输形式
type LineData struct {
    Db        string `json:"db"`
    Line      []byte `json:"line"`
    Precision string `json:"precision"`
}

// MigrationInfo 迁移信息
type MigrationInfo struct {
    CircleNum           int         `json:"circle_num"`
    Database            string      `json:"database"`
    Measurement         string      `json:"measurement"`
    BackendMeasureTotal int         `json:"backend_measure_total"`
    BMNeedMigrateCount  int         `json:"bm_need_migrate_count"`
    BMNotMigrateCount   int         `json:"bm_not_migrate_count"`
}

// NewCluster 新建集群
func NewProxy(file string) (proxy *Proxy, err error) {
    proxy, err = loadProxyJson(file)
    if err != nil {
        return
    }
    proxy.BackendRebalanceStatus = make([]map[string]*MigrationInfo, len(proxy.Circles))
    proxy.BackendRecoveryStatus = make([]map[string]*MigrationInfo, len(proxy.Circles))
    proxy.BackendResyncStatus = make([]map[string]*MigrationInfo, len(proxy.Circles))
    util.CheckPathAndCreate(proxy.DataDir)
    if proxy.MigrateMaxCpus == 0 {
        proxy.MigrateMaxCpus = 1
    }
    for circleNum, circle := range proxy.Circles {
        circle.CircleNum = circleNum
        proxy.initMigration(circle, circleNum)
        proxy.initCircle(circle)
    }
    proxy.DbMap = make(map[string]bool)
    for _, db := range proxy.DbList {
        proxy.DbMap[db] = true
    }
    proxy.ForbidQuery(util.ForbidCmds)
    proxy.EnsureQuery(util.SupportCmds)
    proxy.ClusterQuery(util.ClusterCmds)
    return
}

// loadProxyJson 加载主机配置
func loadProxyJson(file string) (proxy *Proxy, err error) {
    proxy = &Proxy{}
    f, err := os.Open(file)
    defer f.Close()
    if err != nil {
        return
    }
    dec := json.NewDecoder(f)
    err = dec.Decode(proxy)
    return
}

// initCircle 初始化哈希环
func (proxy *Proxy) initCircle(circle *Circle) {
    circle.Router = consistent.New()
    circle.Router.NumberOfReplicas = proxy.VNodeSize
    circle.UrlToBackend = make(map[string]*Backend)
    circle.BackendWgMap = make(map[string]*sync.WaitGroup)
    circle.WgMigrate = &sync.WaitGroup{}
    circle.ReadyMigrating = false
    circle.IsMigrating = false
    circle.StatusLock = &sync.RWMutex{}
    for _, backend := range circle.Backends {
        circle.BackendWgMap[backend.Url] = &sync.WaitGroup{}
        proxy.initBackend(circle, backend)
    }
}

func (proxy *Proxy) initBackend(circle *Circle, backend *Backend) {
    circle.Router.Add(backend.Url)
    circle.UrlToBackend[backend.Url] = backend

    backend.BufferMap = make(map[string]*BufferCounter)
    backend.LockDbMap = make(map[string]*sync.RWMutex)
    backend.LockFile = &sync.RWMutex{}
    backend.Client = &http.Client{}
    backend.Transport = &http.Transport{}
    backend.Active = true
    backend.CreateCacheFile(proxy.DataDir)

    for _, db := range proxy.DbList {
        backend.LockDbMap[db] = new(sync.RWMutex)
        backend.BufferMap[db] = &BufferCounter{Buffer: &bytes.Buffer{}}
    }
    go backend.CheckActive()
    go backend.CheckBufferAndSync(proxy.FlushTime)
    go backend.SyncFileData()
}

func (proxy *Proxy) initMigration(circle *Circle, circleNum int) {
    proxy.BackendRebalanceStatus[circleNum] = make(map[string]*MigrationInfo)
    proxy.BackendRecoveryStatus[circleNum] = make(map[string]*MigrationInfo)
    proxy.BackendResyncStatus[circleNum] = make(map[string]*MigrationInfo)
    for _, backend := range circle.Backends {
        proxy.BackendRebalanceStatus[circleNum][backend.Url] = &MigrationInfo{}
        proxy.BackendRecoveryStatus[circleNum][backend.Url] = &MigrationInfo{}
        proxy.BackendResyncStatus[circleNum][backend.Url] = &MigrationInfo{}
    }
}

// GetMachines 获取数据对应的三台备份物理主机
func (proxy *Proxy) GetMachines(key string) []*Backend {
    machines := make([]*Backend, 0)
    for circleNum, circle := range proxy.Circles {
        backendUrl, err := circle.Router.Get(key)
        if err != nil {
            log.Printf("circleNum:%v key:%+v err:%v", circleNum, key, err)
            continue
        }
        backend := circle.UrlToBackend[backendUrl]
        machines = append(machines, backend)
    }
    return machines
}

// WriteData 写入数据操作
func (proxy *Proxy) WriteData(data *LineData) {
    // 根据 Precision 对line做相应的调整
    data.Line = LineToNano(data.Line, data.Precision)

    measure, err := ScanKey(data.Line)
    if err != nil {
        log.Printf("scan key error: %s\n", err)
        return
    }
    // 得到对象将要存储的多个备份节点
    key := data.Db + "," + measure
    backends := proxy.GetMachines(key)
    // fmt.Printf("%s key: %s; backends:", time.Now().Format("2006-01-02 15:04:05"), key)
    // for _, be := range backends {
    //     fmt.Printf(" %s", be.Name)
    // }
    // fmt.Printf("\n")
    if len(backends) < 1 {
        log.Printf("request data:%v err:GetMachines length is 0", data)
        return
    }

    // 对象如果不是以\n结束的，则加上\n
    if !bytes.HasSuffix(data.Line, []byte("\n")) {
        data.Line = append(data.Line, []byte("\n")...)
    }

    // 顺序存储到多个备份节点上
    for _, backend := range backends {
        err := backend.WriteDataToBuffer(data, proxy.FlushSize)
        if err != nil {
            log.Print("write data to buffer: ", backend.Url, data, err)
            return
        }
    }
    return
}

func (proxy *Proxy) AddBackend(circleNum int) ([]*Backend, error) {
    circle := proxy.Circles[circleNum]
    var res []*Backend
    for _, v := range circle.UrlToBackend {
        res = append(res, v)
    }
    return res, nil
}

func (proxy *Proxy) DeleteBackend(backendUrls []string) ([]*Backend, error) {
    var res []*Backend
    for _, v := range backendUrls {
        res = append(res, &Backend{Url: v, Client: &http.Client{}, Transport: &http.Transport{}})
    }
    return res, nil
}

func (proxy *Proxy) ForbidQuery(cmds []string) {
    for _, cmd := range cmds {
        r, err := regexp.Compile(cmd)
        if err != nil {
            panic(err)
            return
        }
        proxy.ForbiddenQuery = append(proxy.ForbiddenQuery, r)
    }
}

func (proxy *Proxy) EnsureQuery(cmds []string) {
    for _, cmd := range cmds {
        r, err := regexp.Compile(cmd)
        if err != nil {
            panic(err)
            return
        }
        proxy.ObligatedQuery = append(proxy.ObligatedQuery, r)
    }
}
func (proxy *Proxy) ClusterQuery(cmds []string) {
    for _, cmd := range cmds {
        r, err := regexp.Compile(cmd)
        if err != nil {
            panic(err)
            return
        }
        proxy.ClusteredQuery = append(proxy.ClusteredQuery, r)
    }
}

func (proxy *Proxy) CheckMeasurementQuery(q string) bool {
    if len(proxy.ForbiddenQuery) != 0 {
        for _, fq := range proxy.ForbiddenQuery {
            if fq.MatchString(q) {
                return false
            }
        }
    }
    if len(proxy.ObligatedQuery) != 0 {
        for _, pq := range proxy.ObligatedQuery {
            if pq.MatchString(q) {
                return true
            }
        }
        return false
    }
    return true
}

func (proxy *Proxy) CheckClusterQuery(q string) bool {
    if len(proxy.ClusteredQuery) != 0 {
        for _, pq := range proxy.ClusteredQuery {
            if pq.MatchString(q) {
                return true
            }
        }
        return false
    }
    return true
}

func (proxy *Proxy) CheckCreateDatabaseQuery(q string) bool {
    if len(proxy.ClusteredQuery) != 0 {
        return proxy.ClusteredQuery[len(proxy.ClusteredQuery)-1].MatchString(q)
    }
    return false
}

func (proxy *Proxy) CreateDatabase(w http.ResponseWriter, req *http.Request) ([]byte, error) {
    var body []byte
    var err error
    for _, circle := range proxy.Circles {
        body, err = circle.QueryCluster(w, req)
        if err != nil {
            return nil, err
        }
    }
    return body, nil
}

func (proxy *Proxy) Clear(dbs []string, circleNum int) error {
    circle := proxy.Circles[circleNum]
    for _, backend := range circle.Backends {
        circle.WgMigrate.Add(1)
        go proxy.clearCircle(circle, backend, dbs)
    }
    circle.WgMigrate.Wait()
    return nil
}

func (proxy *Proxy) clearCircle(circle *Circle, backend *Backend, dbs []string) {
    defer circle.WgMigrate.Done()

    for _, db := range dbs {
        measures := backend.GetMeasurements(db)
        fmt.Printf("len-->%d db-->%+v\n", len(measures), db)
        for _, measure := range measures {
            key := db + "," + measure
            targetBackendUrl, err := circle.Router.Get(key)
            if err != nil {
                log.Printf("err:%+v")
                continue
            }

            if targetBackendUrl != backend.Url {
                fmt.Printf("src:%+v target:%+v \n", backend.Url, targetBackendUrl)
                _, e := backend.DropMeasurement(db, measure)
                if e != nil {
                    log.Printf("err:%+v", e)
                    continue
                }
            }
        }
    }
}

func (proxy *Proxy) Migrate(backend *Backend, dstBackends []*Backend, circle *Circle, db, measure string, lastSeconds int) {
    err := circle.Migrate(backend, dstBackends, db, measure, lastSeconds)
    if err != nil {
        log.Printf("err:%+v", err)
        return
    }
    circle.BackendWgMap[backend.Url].Done()
}

func (proxy *Proxy) ClearMigrateStatus(data map[string]*MigrationInfo) {
    for _, backend := range data {
        backend.Database = ""
        backend.Measurement = ""
        backend.BackendMeasureTotal = 0
        backend.BMNeedMigrateCount = 0
        backend.BMNotMigrateCount = 0
    }
}

func (proxy *Proxy) Rebalance(backends []*Backend, circleNum int, databases []string) {
    util.SetMLog("./log/rebalance.log", "Rebalance: ")
    circle := proxy.Circles[circleNum]
    circle.SetIsMigrating(true)
    if len(databases) == 0 {
        databases = proxy.DbList
    }

    for _, backend := range backends {
        circle.WgMigrate.Add(1)
        go proxy.RebalanceBackend(backend, circleNum, databases)
    }
    circle.WgMigrate.Wait()
    circle.SetIsMigrating(false)
    util.Mlog.Printf("rebalance done")
}

func (proxy *Proxy) RebalanceBackend(backend *Backend, circleNum int, databases []string) {
    var migrateCount int
    proxy.ClearMigrateStatus(proxy.BackendRebalanceStatus[circleNum])
    circle := proxy.Circles[circleNum]
    defer circle.WgMigrate.Done()

    proxy.BackendRebalanceStatus[circleNum][backend.Url].CircleNum = circleNum
    for _, db := range databases {
        proxy.BackendRebalanceStatus[circleNum][backend.Url].Database = db
        measures := backend.GetMeasurements(db)
        proxy.BackendRebalanceStatus[circleNum][backend.Url].BackendMeasureTotal = len(measures)

        for _, measure := range measures {
            proxy.BackendRebalanceStatus[circleNum][backend.Url].Measurement = measure
            dbMeasure := strings.Join([]string{db, ",", measure}, "")
            targetBackendUrl, err := circle.Router.Get(dbMeasure)
            if err != nil {
                log.Printf("router get error: %s(%d) %s %s %s", circle.Name, circle.CircleNum, db, measure, err)
            }
            if targetBackendUrl != backend.Url {
                proxy.BackendRebalanceStatus[circleNum][backend.Url].BMNeedMigrateCount++
                util.Mlog.Printf("src:%s dst:%s db:%s measurement:%s", backend.Url, targetBackendUrl, db, measure)
                dstBackend := circle.UrlToBackend[targetBackendUrl]
                migrateCount++
                circle.BackendWgMap[backend.Url].Add(1)
                go proxy.Migrate(backend, []*Backend{dstBackend}, circle, db, measure, 0)
                if migrateCount % proxy.MigrateMaxCpus == 0 {
                    circle.BackendWgMap[backend.Url].Wait()
                }
            } else {
                proxy.BackendRebalanceStatus[circleNum][backend.Url].BMNotMigrateCount++
            }
        }
    }
}

func (proxy *Proxy) Recovery(fromCircleNum, toCircleNum int, recoveryBackendUrls []string, databases []string) {
    util.SetMLog("./log/recovery.log", "Recovery: ")
    fromCircle := proxy.Circles[fromCircleNum]
    toCircle := proxy.Circles[toCircleNum]

    fromCircle.SetIsMigrating(true)
    toCircle.SetIsMigrating(true)
    defer fromCircle.SetIsMigrating(false)
    defer toCircle.SetIsMigrating(false)
    if len(databases) == 0 {
        databases = proxy.DbList
    }
    recoveryBackendUrlMap := make(map[string]bool)
    for _, v := range recoveryBackendUrls {
        recoveryBackendUrlMap[v] = true
    }
    for _, backend := range fromCircle.UrlToBackend {
        fromCircle.WgMigrate.Add(1)
        go proxy.RecoveryBackend(backend, fromCircle, toCircle, recoveryBackendUrlMap, databases)
    }
    fromCircle.WgMigrate.Wait()
    util.Mlog.Printf("recovery done")
}

func (proxy *Proxy) RecoveryBackend(backend *Backend, fromCircle, toCircle *Circle, recoveryBackendUrlMap map[string]bool, databases []string) {
    var migrateCount int
    defer fromCircle.WgMigrate.Done()
    fromCircleNum := fromCircle.CircleNum

    proxy.ClearMigrateStatus(proxy.BackendRecoveryStatus[fromCircleNum])
    proxy.BackendRecoveryStatus[fromCircleNum][backend.Url].CircleNum = fromCircleNum
    for _, db := range databases {
        proxy.BackendRecoveryStatus[fromCircleNum][backend.Url].Database = db
        measures := backend.GetMeasurements(db)
        proxy.BackendRecoveryStatus[fromCircleNum][backend.Url].BackendMeasureTotal = len(measures)
        for _, measure := range measures {
            proxy.BackendRecoveryStatus[fromCircleNum][backend.Url].Measurement = measure
            dbMeasure := strings.Join([]string{db, ",", measure}, "")
            targetBackendUrl, err := toCircle.Router.Get(dbMeasure)
            if err != nil {
                log.Printf("router get error: %s(%d) %s(%d) %s %s %s", fromCircle.Name, fromCircle.CircleNum, toCircle.Name, toCircle.CircleNum, db, measure, err)
                continue
            }
            if _, ok := recoveryBackendUrlMap[targetBackendUrl]; ok {
                proxy.BackendRecoveryStatus[fromCircleNum][backend.Url].BMNeedMigrateCount++
                util.Mlog.Printf("src:%s dst:%s db:%s measurement:%s", backend.Url, targetBackendUrl, db, measure)
                dstBackend := toCircle.UrlToBackend[targetBackendUrl]
                migrateCount++
                fromCircle.BackendWgMap[backend.Url].Add(1)
                go proxy.Migrate(backend, []*Backend{dstBackend}, fromCircle, db, measure, 0)
                if migrateCount % proxy.MigrateMaxCpus == 0 {
                    fromCircle.BackendWgMap[backend.Url].Wait()
                }
            } else {
                proxy.BackendRecoveryStatus[fromCircleNum][backend.Url].BMNotMigrateCount++
            }
        }
    }
}

func (proxy *Proxy) Resync(databases []string, lastSeconds int) {
    util.SetMLog("./log/resync.log", "Resync: ")
    if len(databases) == 0 {
        databases = proxy.DbList
    }
    for _, circle := range proxy.Circles {
        circle.SetIsMigrating(true)
        for _, backend := range circle.UrlToBackend {
            circle.WgMigrate.Add(1)
            go proxy.ResyncBackend(backend, circle, databases, lastSeconds)
        }
        circle.WgMigrate.Wait()
        circle.SetIsMigrating(false)
        util.Mlog.Printf("resync %s(circle %d) done", circle.Name, circle.CircleNum)
    }
    util.Mlog.Printf("resync done")
}

func (proxy *Proxy) ResyncBackend(backend *Backend, circle *Circle, databases []string, lastSeconds int) {
    var migrateCount int
    defer circle.WgMigrate.Done()
    circleNum := circle.CircleNum

    proxy.ClearMigrateStatus(proxy.BackendResyncStatus[circleNum])
    proxy.BackendResyncStatus[circleNum][backend.Url].CircleNum = circleNum
    for _, db := range databases {
        proxy.BackendResyncStatus[circleNum][backend.Url].Database = db
        measures := backend.GetMeasurements(db)
        proxy.BackendResyncStatus[circleNum][backend.Url].BackendMeasureTotal = len(measures)
        for _, measure := range measures {
            proxy.BackendResyncStatus[circleNum][backend.Url].Measurement = measure
            dbMeasure := strings.Join([]string{db, ",", measure}, "")
            dstBackends := make([]*Backend, 0)
            for _, toCircle := range proxy.Circles {
                if toCircle.CircleNum != circleNum {
                    targetBackendUrl, err := toCircle.Router.Get(dbMeasure)
                    if err != nil {
                        log.Printf("router get error: %s(%d) %s(%d) %s %s %s", circle.Name, circle.CircleNum, toCircle.Name, toCircle.CircleNum, db, measure, err)
                        continue
                    }
                    dstBackend := toCircle.UrlToBackend[targetBackendUrl]
                    dstBackends = append(dstBackends, dstBackend)
                }
            }
            if len(dstBackends) > 0 {
                proxy.BackendResyncStatus[circleNum][backend.Url].BMNeedMigrateCount++
                util.Mlog.Printf("src:%s dst:%d db:%s measurement:%s", backend.Url, len(dstBackends), db, measure)
                migrateCount++
                circle.BackendWgMap[backend.Url].Add(1)
                go proxy.Migrate(backend, dstBackends, circle, db, measure, lastSeconds)
                if migrateCount % proxy.MigrateMaxCpus == 0 {
                    circle.BackendWgMap[backend.Url].Wait()
                }
            } else {
                proxy.BackendResyncStatus[circleNum][backend.Url].BMNotMigrateCount++
            }
        }
    }
}
