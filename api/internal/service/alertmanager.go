package service

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/ego-component/egorm"
	"github.com/gotomicro/cetus/l"
	"github.com/gotomicro/ego/core/elog"
	"github.com/pkg/errors"

	"github.com/clickvisual/clickvisual/api/internal/invoker"
	"github.com/clickvisual/clickvisual/api/internal/pkg/model/db"
	"github.com/clickvisual/clickvisual/api/internal/pkg/model/view"
	"github.com/clickvisual/clickvisual/api/internal/service/alarm/pusher"
	"github.com/clickvisual/clickvisual/api/internal/service/inquiry/factory"
)

// HandlerAlertManager Processing Alarms
func (i *alert) HandlerAlertManager(alarmUUID string, filterIdStr string, notification db.Notification) (err error) {
	log := elog.With(l.A("alarmUUID", alarmUUID), l.A("filterIdStr", filterIdStr), l.A("notification", notification), elog.FieldMethod("HandlerAlertManager"))
	alarmUUID = strings.ReplaceAll(alarmUUID, "\u0000", "")
	filterIdStr = strings.ReplaceAll(filterIdStr, "\u0000", "")
	// Getting Alarm Information
	tx := invoker.Db.Begin()
	conds := egorm.Conds{}
	conds["uuid"] = alarmUUID
	alarm, err := db.AlarmInfoX(tx, conds)
	if err != nil {
		tx.Rollback()
		return fmt.Errorf("AlarmInfoX %s, error: %w", alarmUUID, err)
	}
	if alarm.Status == db.AlarmStatusClose {
		log.Warn("AlarmStatusClose")
		tx.Commit()
		return
	}
	notificationStatus := notification.GetStatus() // 当前需要推送的状态
	if alarm.IsDisableResolve == 1 && notificationStatus == db.AlarmStatusNormal {
		log.Warn("AlarmIsDisableResolve", l.A("alarm", alarm))
		tx.Commit()
		return
	}
	// create history
	filterId, err := strconv.Atoi(filterIdStr)
	if err != nil {
		tx.Rollback()
		return fmt.Errorf("strconv.Atoi %s, error: %w", filterIdStr, err)
	}
	alarmHistory := db.AlarmHistory{AlarmId: alarm.ID, FilterId: filterId, FilterStatus: notificationStatus, IsPushed: db.PushedStatusRepeat}
	if err = db.AlarmHistoryCreate(tx, &alarmHistory); err != nil {
		tx.Rollback()
		return fmt.Errorf("AlarmHistoryCreate %s, error: %w", alarmUUID, err)
	}
	currentFiltersStatus := alarm.GetStatus(tx)
	// update filter
	af := db.AlarmFilter{}
	af.ID = filterId
	af.Status = notificationStatus
	if err = af.UpdateStatus(tx); err != nil {
		tx.Rollback()
		return fmt.Errorf("alarm filter UpdateStatus %s, error: %w", alarmUUID, err)
	}
	if err = alarm.UpdateStatus(tx, alarm.GetStatus(tx)); err != nil {
		tx.Rollback()
		return fmt.Errorf("alarm UpdateStatus %s, error: %w", alarmUUID, err)
	}
	if currentFiltersStatus == notificationStatus && time.Now().Unix()-alarm.Utime < 300 {
		// 此时有正在进行中的告警
		log.Info("PushAlertManagerRepeat", l.I("filterId", filterId))
		tx.Commit()
		return
	}
	// 完成告警状态更新
	tx.Commit()
	// get alarm filter info
	filter, err := i.compatibleFilter(alarm.ID, filterId)
	if err != nil {
		return fmt.Errorf("compatibleFilter %s, error: %w", alarmUUID, err)
	}
	// get table info
	tableInfo, err := db.TableInfo(invoker.Db, filter.Tid)
	if err != nil {
		return fmt.Errorf("TableInfo %s, error: %w", alarmUUID, err)
	}
	if tableInfo.TimeField == "" {
		tableInfo.TimeField = db.TimeFieldSecond
	}
	// get op
	op, err := InstanceManager.Load(tableInfo.Database.Iid)
	if err != nil {
		return fmt.Errorf("InstanceManager.Load %s, error: %w", alarmUUID, err)
	}
	// get partial log
	partialLog := i.getPartialLog(op, &tableInfo, &alarm, filter)

	pushMsg, err := pusher.BuildAlarmMsg(notification, &tableInfo, &alarm, filter, partialLog)
	if err != nil {
		return fmt.Errorf("BuildAlarmMsg %s, error: %w", alarmUUID, err)
	}

	pushMsgWithAt, err := pusher.BuildAlarmMsgWithAt(notification, &tableInfo, &alarm, filter, partialLog)
	if err != nil {
		return fmt.Errorf("BuildAlarmMsgWithAt %s, error: %w", alarmUUID, err)
	}

	if err = pusher.Execute(alarm.ChannelIds, pushMsg, pushMsgWithAt); err != nil {
		_ = db.AlarmHistoryUpdate(invoker.Db, alarmHistory.ID, map[string]interface{}{"is_pushed": db.PushedStatusFail})
		return fmt.Errorf("execute %s, error: %w", alarmUUID, err)
	}

	if err = db.AlarmHistoryUpdate(invoker.Db, alarmHistory.ID, map[string]interface{}{"is_pushed": db.PushedStatusSuccess}); err != nil {
		return fmt.Errorf("AlarmHistoryUpdate %s, error: %w", alarmUUID, err)
	}
	return nil
}

func (i *alert) compatibleFilter(alarmId int, filterId int) (res *db.AlarmFilter, err error) {
	if filterId == 0 {
		condsFilter := egorm.Conds{}
		condsFilter["alarm_id"] = alarmId
		filters, errAlarmFilterList := db.AlarmFilterList(invoker.Db, condsFilter)
		if errAlarmFilterList != nil {
			return nil, errors.WithMessagef(errAlarmFilterList, "AlarmFilterList %d", alarmId)
		}
		if len(filters) == 0 {
			return nil, errors.New("empty alarm filter")
		}
		res = filters[0]
	} else {
		filter, errAlarmFilterInfo := db.AlarmFilterInfo(invoker.Db, filterId)
		if errAlarmFilterInfo != nil {
			return nil, errors.WithMessagef(errAlarmFilterInfo, "AlarmFilterInfo %d", filterId)
		}
		res = &filter
	}
	return
}

func (i *alert) getPartialLog(op factory.Operator, table *db.BaseTable, alarm *db.Alarm, filter *db.AlarmFilter) (partialLog string) {
	param := view.ReqQuery{
		Tid:           table.ID,
		Database:      table.Database.Name,
		Table:         table.Name,
		Query:         filter.When,
		AlarmMode:     filter.Mode,
		TimeField:     table.TimeField,
		TimeFieldType: table.TimeFieldType,
		ST:            time.Now().Add(-alarm.GetInterval() - time.Minute).Unix(),
		ET:            time.Now().Add(time.Minute).Unix(),
		Page:          1,
		PageSize:      1,
	}
	param, _ = op.Prepare(param, table, false)
	resp, _ := op.GetLogs(param, table.ID)
	if table.V3TableType == db.V3TableTypeJaegerJSON {
		resp.IsTrace = 1
	}
	if len(resp.Logs) > 0 {
		l, _ := json.Marshal(resp.Logs[0])
		partialLog = string(l)
	}
	return partialLog
}
