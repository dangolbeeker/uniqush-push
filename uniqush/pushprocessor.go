/*
 *  Uniqush by Nan Deng
 *  Copyright (C) 2010 Nan Deng
 *
 *  This software is free software; you can redistribute it and/or
 *  modify it under the terms of the GNU Lesser General Public
 *  License as published by the Free Software Foundation; either
 *  version 3.0 of the License, or (at your option) any later version.
 *
 *  This software is distributed in the hope that it will be useful,
 *  but WITHOUT ANY WARRANTY; without even the implied warranty of
 *  MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the GNU
 *  Lesser General Public License for more details.
 *
 *  You should have received a copy of the GNU Lesser General Public
 *  License along with this software; if not, write to the Free Software
 *  Foundation, Inc., 59 Temple Place, Suite 330, Boston, MA  02111-1307  USA
 *
 *  Nan Deng <monnand@gmail.com>
 *
 */

package uniqush

import (
    "log"
    "time"
)

type retryPushTask struct {
    TaskTime
    req *Request
    backendch chan<- *Request
}

func (t *retryPushTask) Run(time int64) {
    t.backendch <- t.req
}

type PushProcessor struct {
    loggerEventWriter
    databaseSetter
    pushers []Pusher
    max_nr_gorountines int
    max_nr_retry int
    q *TaskQueue
    qch chan Task
    backendch chan<- *Request
}

const (
    init_backoff_time = 3E9
)

func (p *PushProcessor) retryRequest(req *Request, retryAfter int, subscriber string, psp *PushServiceProvider, dp *DeliveryPoint) {
    if req.nrRetries >= p.max_nr_retry {
        return
    }
    newreq := new(Request)
    newreq.nrRetries = req.nrRetries + 1
    newreq.PreviousTry = req
    newreq.ID = req.ID
    newreq.Action = ACTION_PUSH
    newreq.PushServiceProvider = psp
    newreq.DeliveryPoint = dp

    newreq.Service = req.Service
    newreq.Subscribers = make([]string, 1)
    newreq.Subscribers[0] = subscriber
    newreq.PunchTimestamp()

    if req.nrRetries == 0 || req.backoffTime == 0 {
        newreq.backoffTime = init_backoff_time
    } else {
        newreq.backoffTime = req.backoffTime << 1
    }

    task := new(retryPushTask)
    task.backendch = p.backendch
    task.req = newreq

    var ra int64
    ra = int64(retryAfter) * 1E9
    exect := time.Nanoseconds() + newreq.backoffTime

    if exect < ra {
        task.execTime = ra
    } else {
        task.execTime = exect
    }

    p.qch <- task
}

func NewPushProcessor(logger *log.Logger, writer *EventWriter, dbfront DatabaseFrontDeskIf, backendch chan<- *Request) RequestProcessor{
    ret := new(PushProcessor)
    ret.SetLogger(logger)
    ret.SetEventWriter(writer)
    ret.SetDatabase(dbfront)
    ret.pushers = make([]Pusher, SRVTYPE_NR_PUSH_SERVICE_TYPE)
    ret.max_nr_gorountines = 1024
    ret.max_nr_retry = 3
    ret.qch = make(chan Task)
    ret.q = NewTaskQueue(ret.qch)
    ret.backendch = backendch

    for i := 0; i < SRVTYPE_NR_PUSH_SERVICE_TYPE; i++ {
        ret.pushers[i] = &NullPusher{}
    }

    ret.pushers[SRVTYPE_C2DM] = NewC2DMPusher()

    return ret
}

func (p *PushProcessor) SetPusher(pushService int, pusher Pusher) {
    if pushService < 0 || pushService >= len(p.pushers) {
        return
    }
    p.pushers[pushService] = pusher
}

type successPushLog struct {
    dp *DeliveryPoint
    subid int
    id string
}

func (p *PushProcessor) pushToSingleDeliveryPoint(req *Request) []*successPushLog{
    ret := make([]*successPushLog, 1)
    ret[0] = nil
    subid := 0
    service := req.Service
    subscriber := req.Subscribers[0]
    psp := req.PushServiceProvider
    dp := req.DeliveryPoint
    i := 0

    if psp.ValidServiceType() {
        id, err := p.pushers[psp.ServiceID()].Push(psp, dp, req.Notification)
        if err != nil {
            switch (err.(type)) {
            case *RefreshDataError:
                re := err.(*RefreshDataError)
                if re.PushServiceProvider != nil {
                    p.dbfront.ModifyPushServiceProvider(re.PushServiceProvider)
                    defer p.logger.Printf("[UpdatePushServiceProvider] Service=%s PushServiceProvider=%s", service, re.PushServiceProvider.Name)
                }
                if re.DeliveryPoint != nil {
                    p.dbfront.ModifyDeliveryPoint(re.DeliveryPoint)
                    defer p.logger.Printf("[UpdateDeliveryPoint] DeliveryPoint=%s", re.DeliveryPoint.Name)
                }
                if re.OtherError != nil {
                    err = re.OtherError
                } else {
                    ret[i] = &successPushLog{dp, subid, id}
                    return ret
                }
            }
            switch (err.(type)) {
            case *RetryError:
                re := err.(*RetryError)
                p.retryRequest(req,re.RetryAfter, subscriber, psp, dp)
                defer p.logger.Printf("[PushRetry] Service=%s Subscriber=%s DeliveryPoint=%s \"%v\"", service, subscriber, dp.Name, err)
                return ret
            }
            // We want to be fast. So defer all IO operations
            defer p.logger.Printf("[PushFail] Service=%s Subscriber=%s DeliveryPoint=%s \"%v\"", service, subscriber, dp.Name, err)
            defer p.writer.PushFail(req, subscriber, dp, err)
            ret[i] = nil
            return ret
        }
        ret[0] = &successPushLog{dp, subid, id}
    }
    return ret
}

func (p *PushProcessor) push(req *Request, service string, subscriber string, subid int) []*successPushLog {
    pspdppairs, err := p.dbfront.GetPushServiceProviderDeliveryPointPairs(service, subscriber)
    if err != nil {
        p.logger.Printf("[PushFail] Service=%s Subscriber=%s DatabaseError %v", service, subscriber, err)
        p.writer.PushFail(req, subscriber, nil, err)
    }
    if len(pspdppairs) <= 0 {
        return nil
        /* Do we need to log this ??
        p.logger.Printf("[PushFail] Service=%s Subscriber=%s NoSubscriber", service, subscriber)
        */
    }
    ret := make([]*successPushLog, len(pspdppairs))

    for i, pdpair := range pspdppairs {
        psp := pdpair.PushServiceProvider
        dp := pdpair.DeliveryPoint

        if psp.ValidServiceType() {
            id, err := p.pushers[psp.ServiceID()].Push(psp, dp, req.Notification)
            if err != nil {
                switch (err.(type)) {
                case *RefreshDataError:
                    re := err.(*RefreshDataError)
                    if re.PushServiceProvider != nil {
                        p.dbfront.ModifyPushServiceProvider(re.PushServiceProvider)
                        defer p.logger.Printf("[UpdatePushServiceProvider] Service=%s PushServiceProvider=%s", service, re.PushServiceProvider.Name)
                    }
                    if re.DeliveryPoint != nil {
                        p.dbfront.ModifyDeliveryPoint(re.DeliveryPoint)
                        defer p.logger.Printf("[UpdateDeliveryPoint] DeliveryPoint=%s", re.DeliveryPoint.Name)
                    }
                    if re.OtherError != nil {
                        err = re.OtherError
                    } else {
                        ret[i] = &successPushLog{dp, subid, id}
                        continue
                    }
                }
                switch (err.(type)) {
                case *RetryError:
                    re := err.(*RetryError)
                    p.retryRequest(req,re.RetryAfter, subscriber, psp, dp)
                    defer p.logger.Printf("[PushRetry] Service=%s Subscriber=%s DeliveryPoint=%s \"%v\"", service, subscriber, dp.Name, err)
                    continue
                }
                // We want to be fast. So defer all IO operations
                defer p.logger.Printf("[PushFail] Service=%s Subscriber=%s DeliveryPoint=%s \"%v\"", service, subscriber, dp.Name, err)
                defer p.writer.PushFail(req, subscriber, dp, err)
                ret[i] = nil
                continue
            }
            ret[i] = &successPushLog{dp, subid, id}
        }
    }

    return ret
}

func (p *PushProcessor) pushBulk(req *Request, service string, subscribers []string, finish chan bool) {
    dps := make([]*successPushLog, 0, len(subscribers) * 2)
    for i, sub := range subscribers {
        r := p.push(req, service, sub, i)
        dps = append(dps, r...)
    }

    // Delay IO operations after real push
    for _, pushlog := range dps {
        if pushlog != nil {
            p.logger.Printf("[PushRequest] Success Service=%s Subscriber=%s DeliveryPoint=%s id=%s",
                        service, subscribers[pushlog.subid], pushlog.dp.Name, pushlog.id)
            p.writer.PushSuccess(req, subscribers[pushlog.subid], pushlog.dp, pushlog.id)

        }
    }
    if finish != nil {
        finish <- true
    }
}

func (p *PushProcessor) Process(req *Request) {
    nr_subs_per_goroutine := len(req.Subscribers) / p.max_nr_gorountines
    nr_subs_last_goroutine := len(req.Subscribers) % p.max_nr_gorountines
    nr_goroutines := 0
    finish := make(chan bool)
    pos := 0

    p.logger.Printf("[DEBUG] I got push req from service %s to %s", req.Service, req.Subscribers)
    if len(req.Subscribers) == 1 && req.PushServiceProvider != nil && req.DeliveryPoint != nil {
        dps := p.pushToSingleDeliveryPoint(req)
        for _, pushlog := range dps {
            if pushlog != nil {
                p.logger.Printf("[PushRequest] Success Service=%s Subscriber=%s DeliveryPoint=%s id=%s",
                            req.Service, req.Subscribers[0], pushlog.dp.Name, pushlog.id)
                p.writer.PushSuccess(req, req.Subscribers[0], pushlog.dp, pushlog.id)

            }
        }
        return
    }

    for pos = 0; pos < len(req.Subscribers) - nr_subs_last_goroutine; pos += nr_subs_per_goroutine {
        go p.pushBulk(req, req.Service, req.Subscribers[pos:pos + nr_subs_per_goroutine], finish)
        nr_goroutines += 1
    }
    if pos < len(req.Subscribers) {
        p.pushBulk(req, req.Service, req.Subscribers[pos:], nil)
    }

    for i := 0; i < nr_goroutines; i++ {
        <-finish
    }
}
