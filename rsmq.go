package rsmq

import (
	"strconv"
	"strings"
	"time"

	"github.com/go-redis/redis"
	"github.com/pkg/errors"
)

// Unset values are the special values to refer default values of the attributes
const (
	UnsetVt      = ^uint(0)
	UnsetDelay   = ^uint(0)
	UnsetMaxsize = -(int(^uint(0)>>1) - 1)
)

const (
	q      = ":Q"
	queues = "QUEUES"
)

const (
	defaultNs      = "rsmq"
	defaultVt      = 30
	defaultDelay   = 0
	defaultMaxsize = 65536
)

// Errors returned on rsmq operation
var (
	ErrQueueNotFound   = errors.New("queue not found")
	ErrQueueExists     = errors.New("queue exists")
	ErrMessageTooLong  = errors.New("message too long")
	ErrMessageNotFound = errors.New("message not found")
)

var (
	hashPopMessage              = redis.NewScript(scriptPopMessage).Hash()
	hashReceiveMessage          = redis.NewScript(scriptReceiveMessage).Hash()
	hashChangeMessageVisibility = redis.NewScript(scriptChangeMessageVisibility).Hash()
)

// RedisSMQ is the client of rsmq to execute queue and message operations
type RedisSMQ struct {
	client *redis.Client
	ns     string
}

// QueueAttributes contains some attributes and stats of queue
type QueueAttributes struct {
	Vt         uint
	Delay      uint
	Maxsize    int
	TotalRecv  uint64
	TotalSent  uint64
	Created    uint64
	Modified   uint64
	Msgs       uint64
	HiddenMsgs uint64
}

type queueDef struct {
	vt      uint
	delay   uint
	maxsize int
	ts      uint64
	uid     string
	qname   string
}

// QueueMessage contains content and metadata of message received from queue
type QueueMessage struct {
	ID      string
	Message string
	Rc      uint64
	Fr      time.Time
	Sent    time.Time
}

// NewRedisSMQ creates and returns new rsmq client
func NewRedisSMQ(client *redis.Client, ns string) *RedisSMQ {
	if client == nil {
		panic("")
	}

	if ns == "" {
		ns = defaultNs
	}
	if !strings.HasSuffix(ns, ":") {
		ns += ":"
	}

	rsmq := &RedisSMQ{
		client: client,
		ns:     ns,
	}

	client.ScriptLoad(scriptPopMessage)
	client.ScriptLoad(scriptReceiveMessage)
	client.ScriptLoad(scriptChangeMessageVisibility)

	return rsmq
}

// CreateQueue creates a new queue with given attributes
// to create new queue with default attributes:
//
//	err:=redisRsmq.CreateQueue(qname,rsmq.UnsetVt,rsmq.UnsetDelay,rsmq.UnsetMaxsize)
func (rsmq *RedisSMQ) CreateQueue(qname string, vt uint, delay uint, maxsize int) error {
	if vt == UnsetVt {
		vt = defaultVt
	}
	if delay == UnsetDelay {
		delay = defaultDelay
	}
	if maxsize == UnsetMaxsize {
		maxsize = defaultMaxsize
	}

	if err := validateQname(qname); err != nil {
		return err
	}
	if err := validateVt(vt); err != nil {
		return err
	}
	if err := validateDelay(delay); err != nil {
		return err
	}
	if err := validateMaxsize(maxsize); err != nil {
		return err
	}

	t, err := rsmq.client.Time().Result()
	if err != nil {
		return err
	}

	key := rsmq.ns + qname + q

	tx := rsmq.client.TxPipeline()
	r := tx.HSetNX(key, "vt", vt)
	tx.HSetNX(key, "delay", delay)
	tx.HSetNX(key, "maxsize", maxsize)
	tx.HSetNX(key, "created", t.Unix())
	tx.HSetNX(key, "modified", t.Unix())
	if _, err = tx.Exec(); err != nil {
		return err
	}
	if !r.Val() {
		return ErrQueueExists
	}

	_, err = rsmq.client.SAdd(rsmq.ns+queues, qname).Result()
	return err
}

func (rsmq *RedisSMQ) getQueue(qname string, uid bool) (*queueDef, error) {
	key := rsmq.ns + qname + q

	tx := rsmq.client.TxPipeline()

	hmGetSliceCmd := tx.HMGet(key, "vt", "delay", "maxsize")
	timeCmd := tx.Time()
	if _, err := tx.Exec(); err != nil {
		return nil, err
	}

	hmGetValues := hmGetSliceCmd.Val()
	if hmGetValues[0] == nil || hmGetValues[1] == nil || hmGetValues[2] == nil {
		return nil, ErrQueueNotFound
	}

	vt := convertToUnsigned[uint](hmGetValues[0])
	delay := convertToUnsigned[uint](hmGetValues[1])
	maxsize := convertToSigned[int](hmGetValues[2])

	t := timeCmd.Val()

	randUID := ""
	if uid {
		randUID = strconv.FormatInt(t.UnixNano()/1000, 36) + makeID(22)
	}

	return &queueDef{
		vt:      vt,
		delay:   delay,
		maxsize: maxsize,
		ts:      uint64(t.UnixMilli()),
		uid:     randUID,
		qname:   qname,
	}, nil
}

// ListQueues returns the slice consist of the existing queues
func (rsmq *RedisSMQ) ListQueues() ([]string, error) {
	return rsmq.client.SMembers(rsmq.ns + queues).Result()
}

// GetQueueAttributes returns queue attributes
func (rsmq *RedisSMQ) GetQueueAttributes(qname string) (*QueueAttributes, error) {
	if err := validateQname(qname); err != nil {
		return nil, err
	}

	queue, err := rsmq.getQueue(qname, false)
	if err != nil {
		return nil, err
	}

	key := rsmq.ns + qname

	tx := rsmq.client.TxPipeline()
	hmGetSliceCmd := tx.HMGet(key+q, "vt", "delay", "maxsize", "totalrecv", "totalsent", "created", "modified")
	zCardIntCmd := tx.ZCard(key)
	zCountIntCmd := tx.ZCount(key, strconv.FormatInt(int64(queue.ts), 10), "+inf")
	if _, err := tx.Exec(); err != nil {
		return nil, err
	}

	hmGetValues := hmGetSliceCmd.Val()

	vt := convertToUnsigned[uint](hmGetValues[0])
	delay := convertToUnsigned[uint](hmGetValues[1])
	maxsize := convertToSigned[int](hmGetValues[2])
	totalRecv := convertStringToUnsigned[uint64](hmGetValues[3], 0)
	totalSent := convertStringToUnsigned[uint64](hmGetValues[4], 0)
	created := convertToUnsigned[uint64](hmGetValues[5])
	modified := convertToUnsigned[uint64](hmGetValues[6])

	msgs := uint64(zCardIntCmd.Val())
	hiddenMsgs := uint64(zCountIntCmd.Val())

	return &QueueAttributes{
		Vt:         vt,
		Delay:      delay,
		Maxsize:    maxsize,
		TotalRecv:  totalRecv,
		TotalSent:  totalSent,
		Created:    created,
		Modified:   modified,
		Msgs:       msgs,
		HiddenMsgs: hiddenMsgs,
	}, nil

}

// SetQueueAttributes sets queue attributes
// to not change some attributes:
//
//	queAttrib,err:=redisRsmq.CreateQueue(qname,rsmq.UnsetVt,rsmq.UnsetDelay,newMaxsize)
func (rsmq *RedisSMQ) SetQueueAttributes(qname string, vt uint, delay uint, maxsize int) (*QueueAttributes, error) {
	if err := validateQname(qname); err != nil {
		return nil, err
	}

	queue, err := rsmq.getQueue(qname, false)
	if err != nil {
		return nil, err
	}

	if vt == UnsetVt {
		vt = queue.vt
	}
	if delay == UnsetDelay {
		delay = queue.delay
	}
	if maxsize == UnsetMaxsize {
		maxsize = queue.maxsize
	}

	if err := validateVt(vt); err != nil {
		return nil, err
	}
	if err := validateDelay(delay); err != nil {
		return nil, err
	}
	if err := validateMaxsize(maxsize); err != nil {
		return nil, err
	}

	key := rsmq.ns + qname + q

	tx := rsmq.client.TxPipeline()
	tx.HSet(key, "modified", queue.ts)
	tx.HSet(key, "vt", vt)
	tx.HSet(key, "delay", delay)
	tx.HSet(key, "maxsize", maxsize)
	if _, err := tx.Exec(); err != nil {
		return nil, err
	}

	return rsmq.GetQueueAttributes(qname)
}

// Quit closes redis client
func (rsmq *RedisSMQ) Quit() error {
	return rsmq.client.Close()
}

// DeleteQueue deletes queue
func (rsmq *RedisSMQ) DeleteQueue(qname string) error {
	if err := validateQname(qname); err != nil {
		return err
	}

	key := rsmq.ns + qname

	tx := rsmq.client.TxPipeline()
	r := tx.Del(key + q)
	tx.Del(key)
	tx.SRem(rsmq.ns+queues, qname)
	if _, err := tx.Exec(); err != nil {
		return nil
	}
	if r.Val() == 0 {
		return ErrQueueNotFound
	}

	return nil
}

// SendMessage sends message to the queue
// to refer queue delay:
//
//	id,err:=redisRsmq.SendMessage(qname,message,rsmq.UnsetDelay)
func (rsmq *RedisSMQ) SendMessage(qname string, message string, delay uint) (string, error) {
	if err := validateQname(qname); err != nil {
		return "", err
	}

	queue, err := rsmq.getQueue(qname, true)
	if err != nil {
		return "", err
	}

	if delay == UnsetDelay {
		delay = queue.delay
	}

	if err := validateDelay(delay); err != nil {
		return "", err
	}

	if queue.maxsize != -1 && len(message) > queue.maxsize {
		return "", ErrMessageTooLong
	}

	key := rsmq.ns + qname

	tx := rsmq.client.TxPipeline()
	tx.ZAdd(key, redis.Z{
		Score:  float64(queue.ts + uint64(delay)*1000),
		Member: queue.uid,
	})
	tx.HSet(key+q, queue.uid, message)
	tx.HIncrBy(key+q, "totalsent", 1)
	if _, err := tx.Exec(); err != nil {
		return "", err
	}

	return queue.uid, nil
}

// ReceiveMessage receives message from the queue
func (rsmq *RedisSMQ) ReceiveMessage(qname string, vt uint) (*QueueMessage, error) {
	if err := validateQname(qname); err != nil {
		return nil, err
	}

	queue, err := rsmq.getQueue(qname, true)
	if err != nil {
		return nil, err
	}

	if vt == UnsetVt {
		vt = queue.vt
	}

	if err := validateVt(vt); err != nil {
		return nil, err
	}

	key := rsmq.ns + qname

	qvt := strconv.FormatUint(queue.ts+uint64(vt)*1000, 10)
	ct := strconv.FormatUint(queue.ts, 10)

	evalCmd := rsmq.client.EvalSha(hashReceiveMessage, []string{key, ct, qvt})
	return rsmq.createQueueMessage(evalCmd)
}

// PopMessage pop message from queue
func (rsmq *RedisSMQ) PopMessage(qname string) (*QueueMessage, error) {
	if err := validateQname(qname); err != nil {
		return nil, err
	}

	queue, err := rsmq.getQueue(qname, false)
	if err != nil {
		return nil, err
	}

	key := rsmq.ns + qname

	t := strconv.FormatUint(queue.ts, 10)

	evalCmd := rsmq.client.EvalSha(hashPopMessage, []string{key, t})
	return rsmq.createQueueMessage(evalCmd)
}

func (rsmq *RedisSMQ) createQueueMessage(cmd *redis.Cmd) (*QueueMessage, error) {
	vals, ok := cmd.Val().([]any)
	if !ok {
		return nil, errors.New("mismatched message response type")
	}
	if len(vals) == 0 {
		return nil, nil
	}
	id := vals[0].(string)
	message := vals[1].(string)
	rc := convertToUnsigned[uint64](vals[2])
	fr := convertToSigned[int64](vals[3])
	sent, err := strconv.ParseInt(id[0:10], 36, 64)
	if err != nil {
		panic(err)
	}

	return &QueueMessage{
		ID:      id,
		Message: message,
		Rc:      rc,
		Fr:      time.UnixMilli(fr),
		Sent:    time.UnixMicro(sent),
	}, nil
}

// ChangeMessageVisibility changes message visibility
// to refer queue vt
//
//	err:=redisRsmq.ChangeMessageVisibility(qname,id,rsmq.UnsetVt)
func (rsmq *RedisSMQ) ChangeMessageVisibility(qname string, id string, vt uint) error {
	if err := validateQname(qname); err != nil {
		return err
	}
	if err := validateID(id); err != nil {
		return err
	}

	queue, err := rsmq.getQueue(qname, false)
	if err != nil {
		return err
	}

	if vt == UnsetVt {
		vt = queue.vt
	}

	if err := validateVt(vt); err != nil {
		return err
	}

	key := rsmq.ns + qname
	t := strconv.FormatUint(queue.ts+uint64(vt)*1000, 10)

	evalCmd := rsmq.client.EvalSha(hashChangeMessageVisibility, []string{key, id, t})
	if e, err := evalCmd.Bool(); err != nil {
		return err
	} else if !e {
		return ErrMessageNotFound
	}

	return nil
}

// DeleteMessage deletes message in queue
func (rsmq *RedisSMQ) DeleteMessage(qname string, id string) error {
	if err := validateQname(qname); err != nil {
		return err
	}
	if err := validateID(id); err != nil {
		return err
	}

	_, err := rsmq.getQueue(qname, false)
	if err != nil {
		return err
	}

	key := rsmq.ns + qname

	tx := rsmq.client.TxPipeline()
	zremIntCmd := tx.ZRem(key, id)
	hdelIntCmd := tx.HDel(key+q, id, id+":rc", id+":fr")
	if _, err := tx.Exec(); err != nil {
		return err
	}

	if zremIntCmd.Val() != 1 || hdelIntCmd.Val() == 0 {
		return ErrMessageNotFound
	}

	return nil
}
