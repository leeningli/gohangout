package output

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/childe/gohangout/simplejson"
	"github.com/childe/gohangout/value_render"
	"github.com/golang/glog"
)

const (
	DEFAULT_INDEX_TYPE  = "logs"
	META_FORMAT_WITH_ID = `{"%s":{"_index":%s,"_type":%s,"_id":%s,"routing":%s}}` + "\n"
)

type Action struct {
	op         string
	index      string
	index_type string
	id         string
	routing    string
	event      map[string]interface{}
	rawSource  []byte
}

func (action *Action) Encode() []byte {
	var (
		meta []byte = make([]byte, 0, 1000)
		buf  []byte
		err  error
		d    = &simplejson.SimpleJsonDecoder{}
	)
	meta = append(meta, `{"`+action.op+`":{"_index":`...)
	index, _ := d.Encode(action.index)
	meta = append(meta, index...)
	d.Reset()

	meta = append(meta, `,"_type":`...)
	index_type, _ := d.Encode(action.index_type)
	meta = append(meta, index_type...)
	d.Reset()

	if action.id != "" {
		meta = append(meta, `,"_id":`...)
		doc_id, _ := d.Encode(action.id)
		meta = append(meta, doc_id...)
		d.Reset()
	}

	meta = append(meta, `,"routing":`...)
	routing, _ := d.Encode(action.routing)
	meta = append(meta, routing...)
	d.Reset()

	meta = append(meta, "}}\n"...)

	if action.rawSource == nil {
		d.Reset()
		buf, err = d.Encode(action.event)
		if err != nil {
			glog.Errorf("could marshal event(%v):%s", action.event, err)
			return nil
		}
	} else {
		buf = action.rawSource
	}

	bulk_buf := make([]byte, 0, len(meta)+len(buf)+1)
	bulk_buf = append(bulk_buf, meta...)
	bulk_buf = append(bulk_buf, buf[:len(buf)]...)
	bulk_buf = append(bulk_buf, '\n')
	return bulk_buf
}

type ESBulkRequest struct {
	events   []Event
	bulk_buf []byte
}

func (br *ESBulkRequest) add(event Event) {
	br.bulk_buf = append(br.bulk_buf, event.Encode()...)
	br.events = append(br.events, event)
}

func (br *ESBulkRequest) bufSizeByte() int {
	return len(br.bulk_buf)
}
func (br *ESBulkRequest) eventCount() int {
	return len(br.events)
}
func (br *ESBulkRequest) readBuf() []byte {
	return br.bulk_buf
}

type ElasticsearchOutput struct {
	BaseOutput
	config map[interface{}]interface{}

	index              value_render.ValueRender
	index_type         value_render.ValueRender
	id                 value_render.ValueRender
	routing            value_render.ValueRender
	source_field       value_render.ValueRender
	bytes_source_field value_render.ValueRender

	bulkProcessor BulkProcessor
}

func esGetRetryEvents(resp *http.Response, respBody []byte, bulkRequest *BulkRequest) ([]int, []int, BulkRequest) {
	retry := make([]int, 0)
	noRetry := make([]int, 0)

	var responseI interface{}
	err := json.Unmarshal(respBody, &responseI)
	if err != nil {
		glog.Errorf(`could not unmarshal bulk response:"%s". will NOT retry. %s`, err, string(respBody[:100]))
		return retry, noRetry, nil
	}

	bulkResponse := responseI.(map[string]interface{})
	glog.V(20).Infof("%v", bulkResponse)

	if bulkResponse["errors"] == nil {
		glog.Infof("could NOT get errors in response:%s", string(respBody))
		return retry, noRetry, nil
	}

	if bulkResponse["errors"].(bool) == false {
		return retry, noRetry, nil
	}

	hasLog := false
	for i, item := range bulkResponse["items"].([]interface{}) {
		index := item.(map[string]interface{})["index"].(map[string]interface{})

		if errorValue, ok := index["error"]; ok {
			//errorType := errorValue.(map[string]interface{})["type"].(string)
			if !hasLog {
				glog.Infof("error :%v", errorValue)
				hasLog = true
			}

			status := index["status"].(float64)
			if status == 429 || status >= 500 {
				retry = append(retry, i)
			} else {
				noRetry = append(noRetry, i)
			}
		}
	}
	newbulkRequest := buildRetryBulkRequest(retry, noRetry, bulkRequest)
	return retry, noRetry, newbulkRequest
}

func buildRetryBulkRequest(shouldRetry, noRetry []int, bulkRequest *BulkRequest) BulkRequest {
	esBulkRequest := (*bulkRequest).(*ESBulkRequest)
	if len(noRetry) > 0 {
		b, err := json.Marshal(esBulkRequest.events[noRetry[0]].(*Action).event)
		if err != nil {
			glog.Infof("one failed doc that need no retry: %+v", esBulkRequest.events[noRetry[0]].(*Action).event)
		} else {
			glog.Infof("one failed doc that need no retry: %s", b)
		}
	}

	if len(shouldRetry) > 0 {
		newBulkRequest := &ESBulkRequest{
			bulk_buf: make([]byte, 0),
		}
		for _, i := range shouldRetry {
			newBulkRequest.add(esBulkRequest.events[i])
		}
		return newBulkRequest
	}
	return nil
}

func NewElasticsearchOutput(config map[interface{}]interface{}) *ElasticsearchOutput {
	rst := &ElasticsearchOutput{
		BaseOutput: NewBaseOutput(config),
		config:     config,
	}

	if v, ok := config["index"]; ok {
		rst.index = value_render.GetValueRender(v.(string))
	} else {
		glog.Fatal("index must be set in elasticsearch output")
	}

	if v, ok := config["index_type"]; ok {
		rst.index_type = value_render.GetValueRender(v.(string))
	} else {
		rst.index_type = value_render.GetValueRender(DEFAULT_INDEX_TYPE)
	}

	if v, ok := config["id"]; ok {
		rst.id = value_render.GetValueRender(v.(string))
	} else {
		rst.id = nil
	}

	if v, ok := config["routing"]; ok {
		rst.routing = value_render.GetValueRender(v.(string))
	} else {
		rst.routing = nil
	}

	if v, ok := config["source_field"]; ok {
		rst.source_field = value_render.GetValueRender2(v.(string))
	} else {
		rst.source_field = nil
	}

	if v, ok := config["bytes_source_field"]; ok {
		rst.bytes_source_field = value_render.GetValueRender2(v.(string))
	} else {
		rst.bytes_source_field = nil
	}

	var (
		bulk_size, bulk_actions, flush_interval, concurrent int
		compress                                            bool
	)
	if v, ok := config["bulk_size"]; ok {
		bulk_size = v.(int) * 1024 * 1024
	} else {
		bulk_size = DEFAULT_BULK_SIZE
	}

	if v, ok := config["bulk_actions"]; ok {
		bulk_actions = v.(int)
	} else {
		bulk_actions = DEFAULT_BULK_ACTIONS
	}
	if v, ok := config["flush_interval"]; ok {
		flush_interval = v.(int)
	} else {
		flush_interval = DEFAULT_FLUSH_INTERVAL
	}
	if v, ok := config["concurrent"]; ok {
		concurrent = v.(int)
	} else {
		concurrent = DEFAULT_CONCURRENT
	}
	if concurrent <= 0 {
		glog.Fatal("concurrent must > 0")
	}
	if v, ok := config["compress"]; ok {
		compress = v.(bool)
	} else {
		compress = true
	}

	var hosts []string
	if v, ok := config["hosts"]; ok {
		for _, h := range v.([]interface{}) {
			hosts = append(hosts, h.(string)+"/_bulk")
		}
	} else {
		glog.Fatal("hosts must be set in elasticsearch output")
	}

	var headers = map[string]string{"Content-Type": "application/x-ndjson"}
	if v, ok := config["headers"]; ok {
		for keyI, valueI := range v.(map[interface{}]interface{}) {
			headers[keyI.(string)] = valueI.(string)
		}
	}
	var requestMethod string = "POST"

	var retryResponseCode map[int]bool = make(map[int]bool)
	if v, ok := config["retry_response_code"]; ok {
		for _, cI := range v.([]interface{}) {
			retryResponseCode[cI.(int)] = true
		}
	} else {
		retryResponseCode[401] = true
		retryResponseCode[502] = true
	}

	byte_size_applied_in_advance := bulk_size + 1024*1024
	if byte_size_applied_in_advance > MAX_BYTE_SIZE_APPLIED_IN_ADVANCE {
		byte_size_applied_in_advance = MAX_BYTE_SIZE_APPLIED_IN_ADVANCE
	}
	var f = func() BulkRequest {
		return &ESBulkRequest{
			bulk_buf: make([]byte, 0, byte_size_applied_in_advance),
		}
	}

	rst.bulkProcessor = NewHTTPBulkProcessor(headers, hosts, requestMethod, retryResponseCode, bulk_size, bulk_actions, flush_interval, concurrent, compress, f, esGetRetryEvents)
	return rst
}

func (p *ElasticsearchOutput) Emit(event map[string]interface{}) {
	var (
		index      string = p.index.Render(event).(string)
		index_type string = p.index_type.Render(event).(string)
		op         string = "index"
		id         string
		routing    string
	)
	if p.id == nil {
		id = ""
	} else {
		t := p.id.Render(event)
		if t == nil {
			id = ""
			glog.V(20).Infof("could not render id:%s", event)
		} else {
			id = t.(string)
		}
	}

	if p.routing == nil {
		routing = ""
	} else {
		t := p.routing.Render(event)
		if t == nil {
			routing = ""
			glog.V(20).Infof("could not render routing:%s", event)
		} else {
			routing = t.(string)
		}
	}

	if p.source_field == nil && p.bytes_source_field == nil {
		p.bulkProcessor.add(&Action{op, index, index_type, id, routing, event, nil})
	} else if p.bytes_source_field != nil {
		t := p.bytes_source_field.Render(event)
		if t == nil {
			p.bulkProcessor.add(&Action{op, index, index_type, id, routing, event, nil})
		} else {
			p.bulkProcessor.add(&Action{op, index, index_type, id, routing, event, (t.([]byte))})
		}
	} else {
		t := p.source_field.Render(event)
		if t == nil {
			p.bulkProcessor.add(&Action{op, index, index_type, id, routing, event, nil})
		} else {
			p.bulkProcessor.add(&Action{op, index, index_type, id, routing, event, []byte(t.(string))})
		}
	}
}

func (outputPlugin *ElasticsearchOutput) Shutdown() {
	outputPlugin.bulkProcessor.awaitclose(30 * time.Second)
}
