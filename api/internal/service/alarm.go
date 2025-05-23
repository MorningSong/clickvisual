package service

import (
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ego-component/egorm"
	"github.com/gotomicro/ego/core/econf"
	"github.com/gotomicro/ego/core/elog"
	"github.com/pkg/errors"
	"gorm.io/gorm"

	"github.com/clickvisual/clickvisual/api/internal/invoker"
	"github.com/clickvisual/clickvisual/api/internal/pkg/component/core"
	db2 "github.com/clickvisual/clickvisual/api/internal/pkg/model/db"
	"github.com/clickvisual/clickvisual/api/internal/pkg/model/view"
	"github.com/clickvisual/clickvisual/api/internal/service/alarm/alertcomponent"
	"github.com/clickvisual/clickvisual/api/internal/service/alarm/pusher"
	"github.com/clickvisual/clickvisual/api/internal/service/alarm/rule"
	"github.com/clickvisual/clickvisual/api/internal/service/inquiry/factory"
	"github.com/clickvisual/clickvisual/api/internal/service/inquiry/factory/builder/bumo"
)

// const prometheusRuleTemplate = `groups:
// - name: default
//   rules:
//   - alert: ClickVisual-%s
//     expr: %s
//     for: %s
//     labels:
//       severity: warning
//     annotations:
//       summary: "告警 {{ $labels.name }}"
//       description: "{{ $labels.desc }}  (当前值: {{ $value }})"`

const prometheusRuleTemplateWithoutFor = `groups:
- name: default
  rules:
  - alert: ClickVisual-%s
    expr: %s
    labels:
      severity: warning
    annotations:
      summary: "告警 {{ $labels.name }}"
      description: "{{ $labels.desc }}  (当前值: {{ $value }})"`

const (
	reloadTimes    = 30
	reloadInterval = time.Second * 5
)

const (
	NoDataOpDefault = 0
	NoDataOpOK      = 1
	NoDataOpAlert   = 2
)

var _ iAlert = (*alert)(nil)

type iAlert interface {
	FilterCreate(tx *gorm.DB, alarmObj *db2.Alarm, filters []view.ReqAlarmFilterCreate) (res map[int]view.AlarmFilterItem, err error)
	ConditionCreate(tx *gorm.DB, obj *db2.Alarm, conditions []view.ReqAlarmConditionCreate, filter *db2.AlarmFilter) (exp string, err error)
	PrometheusReload(prometheusTarget string) (err error)
	PrometheusRuleGen(obj *db2.Alarm, exp string, filterId int) string
	PrometheusRuleCreateOrUpdate(instance db2.BaseInstance, groupName, ruleName, content string) (err error)
	DeletePrometheusRule(instance *db2.BaseInstance, obj *db2.Alarm) (err error)
	CreateOrUpdate(alarmObj *db2.Alarm, req view.ReqAlarmCreate) (err error)
	OpenOperator(id int) (err error)
	Update(uid, alarmId int, req view.ReqAlarmCreate) (err error)
	AddPrometheusReloadChan()
	IsAllClosed(instanceId int) (err error)
	HandlerAlertManager(alarmUUID string, filterId string, notification db2.Notification) (err error)
}

type alert struct {
	reloadChan chan int64
}

// NewAlarm ...
func NewAlarm() *alert {
	a := &alert{
		reloadChan: make(chan int64, reloadTimes),
	}
	go func() {
		for r := range a.reloadChan {
			elog.Info("AllPrometheusReload", elog.Int("times", len(a.reloadChan)), elog.Int64("r", r), elog.Int64("now", time.Now().Unix()))
			AllPrometheusReload()
			core.LoggerError("alert", "ruleReload", AlertRuleCheck())
			time.Sleep(reloadInterval)
		}
	}()
	a.reloadChan <- time.Now().Unix()
	return a
}

func (i *alert) FilterCreate(tx *gorm.DB, alarmObj *db2.Alarm, filters []view.ReqAlarmFilterCreate) (res map[int]view.AlarmFilterItem, err error) {
	res = make(map[int]view.AlarmFilterItem, 0)
	for _, filter := range filters {
		// create filter
		filterObj := &db2.AlarmFilter{
			AlarmId:        alarmObj.ID,
			Tid:            filter.Tid,
			When:           filter.When,
			SetOperatorTyp: filter.SetOperatorTyp,
			SetOperatorExp: filter.SetOperatorExp,
			Mode:           filter.Mode,
		}
		if filterObj.When == "" {
			filterObj.When = "1=1"
		}
		err = db2.AlarmFilterCreate(tx, filterObj)
		if err != nil {
			return
		}
		row := view.AlarmFilterItem{
			AlarmFilter: filterObj,
		}
		// create condition
		row.Exp, err = i.ConditionCreate(tx, alarmObj, filter.Conditions, filterObj)
		if err != nil {
			return
		}
		res[filterObj.ID] = row
	}
	return
}

func (i *alert) ConditionCreate(tx *gorm.DB, obj *db2.Alarm, conditions []view.ReqAlarmConditionCreate, filter *db2.AlarmFilter) (exp string, err error) {
	expVal := fmt.Sprintf("%s{%s} offset 10s", bumo.PrometheusMetricName, factory.TagsToString(obj, false, filter.ID))
	sort.Slice(conditions, func(i, j int) bool {
		return conditions[i].SetOperatorTyp < conditions[j].SetOperatorTyp
	})
	for _, condition := range conditions {
		var innerCond string
		var ot string
		switch condition.SetOperatorExp {
		case 0:
			ot = "avg_over_time"
		case 1:
			ot = "min_over_time"
		case 2:
			ot = "max_over_time"
		case 3:
			ot = "sum_over_time"
		case 4:
			ot = "count_over_time"
		}
		expValOverTime := fmt.Sprintf("%s(%s{%s}[%s] offset 10s)", ot, bumo.PrometheusMetricName, factory.TagsToString(obj, false, filter.ID), obj.AlertInterval())
		switch condition.Cond {
		case 0:
			innerCond = fmt.Sprintf("%s>%d", expValOverTime, condition.Val1)
		case 1:
			innerCond = fmt.Sprintf("%s<%d", expValOverTime, condition.Val1)
		case 2:
			innerCond = fmt.Sprintf("(%s<%d or %s>%d)", expValOverTime, condition.Val1, expValOverTime, condition.Val2)
		case 3:
			innerCond = fmt.Sprintf("(%s>=%d and %s<=%d)", expValOverTime, condition.Val1, expValOverTime, condition.Val2)
		}
		switch condition.SetOperatorTyp {
		case 0:
			exp = innerCond
		case 1:
			if exp == "" {
				err = errors.New("conditions error")
				return
			}
			exp = fmt.Sprintf("%s and %s", exp, innerCond)
		case 2:
			if exp == "" {
				err = errors.New("conditions error")
				return
			}
			exp = fmt.Sprintf("%s or %s", exp, innerCond)
		}
		conditionObj := &db2.AlarmCondition{
			AlarmId:        obj.ID,
			FilterId:       filter.ID,
			SetOperatorTyp: condition.SetOperatorTyp,
			SetOperatorExp: condition.SetOperatorExp,
			Cond:           condition.Cond,
			Val1:           condition.Val1,
			Val2:           condition.Val2,
		}
		err = db2.AlarmConditionCreate(tx, conditionObj)
		if err != nil {
			return
		}
	}

	// empty data alert
	exp = aggregationOp(filter.Mode, exp, expVal)
	exp = noDataOp(obj.NoDataOp, exp, expVal)
	return
}

func (i *alert) PrometheusReload(prometheusTarget string) (err error) {
	resp, err := http.Post(strings.TrimSuffix(prometheusTarget, "/")+"/-/reload", "text/html;charset=utf-8", nil)
	if err != nil {
		elog.Error("reload", elog.Any("reload", prometheusTarget+"/-/reload"), elog.Any("err", err.Error()))
		return
	}
	defer func() { _ = resp.Body.Close() }()
	return
}

func (i *alert) PrometheusRuleGen(obj *db2.Alarm, exp string, filterId int) string {
	// TODO 后期增加 for 参数
	// return fmt.Sprintf(prometheusRuleTemplate, obj.UniqueName(filterId), exp, obj.AlertInterval())
	return fmt.Sprintf(prometheusRuleTemplateWithoutFor, obj.UniqueName(filterId), exp)
}

func (i *alert) PrometheusRuleCreateOrUpdate(instance db2.BaseInstance, groupName, ruleName, content string) (err error) {
	rc, err := rule.GetComponent(instance.RuleStoreType, &rule.Params{
		InstanceID:         instance.ID,
		RulePath:           instance.FilePath,
		ClusterId:          instance.K8sClusterId,
		Namespace:          instance.K8sNamespace,
		Configmap:          instance.K8sConfigmap,
		PrometheusOperator: instance.ConfigPrometheusOperator,
	})
	if err != nil {
		return err
	}
	if err = rc.CreateOrUpdate(groupName, ruleName, content); err != nil {
		return
	}
	go i.AddPrometheusReloadChan()
	return nil
}

func (i *alert) PrometheusRuleBatchSet(clusterRuleGroups map[string]db2.ClusterRuleGroup) (err error) {
	for _, clusterRuleGroup := range clusterRuleGroups {
		rc, err := rule.GetComponent(clusterRuleGroup.Instance.RuleStoreType, &rule.Params{
			InstanceID:         clusterRuleGroup.Instance.ID,
			RulePath:           clusterRuleGroup.Instance.FilePath,
			ClusterId:          clusterRuleGroup.Instance.K8sClusterId,
			Namespace:          clusterRuleGroup.Instance.K8sNamespace,
			Configmap:          clusterRuleGroup.Instance.K8sConfigmap,
			PrometheusOperator: clusterRuleGroup.Instance.ConfigPrometheusOperator,
		})
		if err != nil {
			return errors.Wrap(err, "k8s configmap write error")
		}
		if err = rc.BatchSet(clusterRuleGroup.GroupName, clusterRuleGroup.Rules); err != nil {
			return err
		}
	}
	go i.AddPrometheusReloadChan()
	return nil
}

func (i *alert) PrometheusRuleBatchRemove(clusterRuleGroups map[string]db2.ClusterRuleGroup) (err error) {
	for _, clusterRuleGroup := range clusterRuleGroups {
		rc, err := rule.GetComponent(clusterRuleGroup.Instance.RuleStoreType, &rule.Params{
			InstanceID:         clusterRuleGroup.Instance.ID,
			RulePath:           clusterRuleGroup.Instance.FilePath,
			ClusterId:          clusterRuleGroup.Instance.K8sClusterId,
			Namespace:          clusterRuleGroup.Instance.K8sNamespace,
			Configmap:          clusterRuleGroup.Instance.K8sConfigmap,
			PrometheusOperator: clusterRuleGroup.Instance.ConfigPrometheusOperator,
		})
		if err != nil {
			return err
		}
		if err = rc.BatchRemove(clusterRuleGroup.GroupName); err != nil {
			return err
		}
	}
	go i.AddPrometheusReloadChan()
	return nil
}

func (i *alert) DeletePrometheusRule(instance *db2.BaseInstance, obj *db2.Alarm) (err error) {
	if instance == nil {
		return fmt.Errorf("instance is nil")
	}
	if obj == nil {
		return fmt.Errorf("alarm is nil")
	}
	if len(obj.AlertRules) == 0 {
		// v1 version
		return alarmRuleDelete(instance, obj.GetGroupName(instance.ID), obj.RuleName(0))
	} else {
		// v2 version
		for iidRuleName := range obj.AlertRules {
			ruleName := iidRuleName
			ins := *instance
			iidTableArr := strings.Split(iidRuleName, "|")
			if len(iidTableArr) == 2 {
				ruleName = iidTableArr[1]
				iid, _ := strconv.Atoi(iidTableArr[0])
				ins, _ = db2.InstanceInfo(invoker.Db, iid)
			}
			if ins.RuleStoreType == db2.RuleStoreTypeK8sOperator {
				continue
			}
			if err = alarmRuleDelete(&ins, obj.GetGroupName(ins.ID), ruleName); err != nil {
				return
			}
		}
	}
	go i.AddPrometheusReloadChan()
	return nil
}

func (i *alert) CreateOrUpdate(alarmObj *db2.Alarm, req view.ReqAlarmCreate) (err error) {
	// v1 -> v2 disable root conditions field
	req.ConvertV2()
	filtersDB, err := i.FilterCreate(invoker.Db, alarmObj, req.Filters)
	if err != nil {
		return
	}
	// create new views
	viewDDLs := db2.String2String{}
	alertRules := db2.String2String{}
	clusterRuleGroups := map[string]db2.ClusterRuleGroup{}
	for filterId, filterItem := range filtersDB {
		var tableInfo db2.BaseTable
		// table info
		tableInfo, err = db2.TableInfo(invoker.Db, filterItem.Tid)
		if err != nil {
			return
		}
		// prometheus set
		var instance db2.BaseInstance
		instance, err = db2.InstanceInfo(invoker.Db, tableInfo.Database.Iid)
		if err != nil {
			return
		}
		var op factory.Operator
		op, err = InstanceManager.Load(tableInfo.Database.Iid)
		if err != nil {
			return
		}
		// drop alert views
		if len(alarmObj.ViewDDLs) > 0 {
			for iidTable := range alarmObj.ViewDDLs {
				ddlOp := op
				table := iidTable
				iidTableArr := strings.Split(iidTable, "|")
				if len(iidTableArr) == 2 {
					table = iidTableArr[1]
					iid, _ := strconv.Atoi(iidTableArr[0])
					ddlOp, err = InstanceManager.Load(iid)
					if err != nil {
						return
					}
					if iid != instance.ID {
						continue
					}
				}
				if err = ddlOp.DeleteAlertView(table, tableInfo.Database.Cluster); err != nil {
					return
				}
			}
		} else {
			if alarmObj.ViewTableName != "" {
				err = op.DeleteAlertView(alarmObj.ViewTableName, tableInfo.Database.Cluster)
				if err != nil {
					elog.Error("alert", elog.String("step", "alert create failed 05"), elog.String("err", err.Error()))
					return
				}
			}
		}
		// gen view table name & sql
		table, ddl, errAlertViewGen := op.GetAlertViewSQL(alarmObj, tableInfo, filterId, &filterItem)
		if errAlertViewGen != nil {
			return errAlertViewGen
		}
		// exec view sql
		if err = op.CreateAlertView(table, ddl, tableInfo.Database.Cluster); err != nil {
			return
		}
		viewDDLs[fmt.Sprintf("%d|%s", tableInfo.Database.Iid, table)] = ddl
		// rule store
		r := i.PrometheusRuleGen(alarmObj, filterItem.Exp, filterId)
		ruleName := alarmObj.RuleName(filterId)
		alertRules[fmt.Sprintf("%d|%s", tableInfo.Database.Iid, ruleName)] = r
		if instance.RuleStoreType == db2.RuleStoreTypeK8sOperator {
			clusterRuleGroup := db2.ClusterRuleGroup{}
			if tmp, ok := clusterRuleGroups[instance.GetRuleStoreKey()]; ok {
				clusterRuleGroup = tmp
			} else {
				clusterRuleGroup.ClusterId = instance.K8sClusterId
				clusterRuleGroup.Instance = instance
				clusterRuleGroup.GroupName = alarmObj.GetGroupName(instance.ID)
				clusterRuleGroup.Rules = make([]db2.ClusterRuleItem, 0)
			}
			clusterRuleGroup.Rules = append(clusterRuleGroup.Rules, db2.ClusterRuleItem{
				RuleName: ruleName,
				Content:  r,
			})
			clusterRuleGroups[instance.GetRuleStoreKey()] = clusterRuleGroup
		} else if instance.RuleStoreType == db2.RuleStoreTypeFile || instance.RuleStoreType == db2.RuleStoreTypeK8sConfigMap {
			if err = Alert.DeletePrometheusRule(&instance, alarmObj); err != nil {
				return
			}
			if err = i.PrometheusRuleCreateOrUpdate(instance, alarmObj.GetGroupName(instance.ID), ruleName, r); err != nil {
				return
			}
		}
	}
	ups := make(map[string]interface{}, 0)
	ups["alert_rules"] = alertRules
	ups["view_ddl_s"] = viewDDLs
	ups["status"] = db2.AlarmStatusRuleCheck
	err = db2.AlarmUpdate(invoker.Db, alarmObj.ID, ups)
	if err != nil {
		return
	}
	if len(clusterRuleGroups) > 0 {
		if err = i.PrometheusRuleBatchSet(clusterRuleGroups); err != nil {
			return
		}
	}
	return nil
}

func (i *alert) OpenOperator(id int) (err error) {
	alarmInfo, relatedList, err := db2.GetAlarmTableInstanceInfo(id)
	if err != nil {
		return
	}
	clusterRuleGroups := map[string]db2.ClusterRuleGroup{}

	for _, ri := range relatedList {
		op, errInstanceManager := InstanceManager.Load(ri.Instance.ID)
		if errInstanceManager != nil {
			return errInstanceManager
		}
		if len(alarmInfo.ViewDDLs) > 0 {
			for iidTable, ddl := range alarmInfo.ViewDDLs {
				table := iidTable
				iidTableArr := strings.Split(iidTable, "|")
				if len(iidTableArr) == 2 {
					table = iidTableArr[1]
					iid, _ := strconv.Atoi(iidTableArr[0])
					op, err = InstanceManager.Load(iid)
					if err != nil {
						return
					}
					if iid != ri.Table.Database.Iid {
						continue
					}
				}
				if err = op.CreateAlertView(table, ddl, ri.Table.Database.Cluster); err != nil {
					return
				}
			}
		} else {
			if err = op.CreateAlertView(alarmInfo.ViewTableName, alarmInfo.View, ri.Table.Database.Cluster); err != nil {
				return
			}
		}
		if len(alarmInfo.AlertRules) > 0 {
			for iidRuleName, alertRule := range alarmInfo.AlertRules {
				ruleName := iidRuleName
				iidTableArr := strings.Split(iidRuleName, "|")
				var instance db2.BaseInstance
				if len(iidTableArr) == 2 {
					ruleName = iidTableArr[1]
					iid, _ := strconv.Atoi(iidTableArr[0])
					instance, _ = db2.InstanceInfo(invoker.Db, iid)
				}
				if instance.RuleStoreType == db2.RuleStoreTypeK8sOperator {
					clusterRuleGroup := db2.ClusterRuleGroup{}
					if tmp, ok := clusterRuleGroups[instance.GetRuleStoreKey()]; ok {
						clusterRuleGroup = tmp
					} else {
						clusterRuleGroup.ClusterId = instance.K8sClusterId
						clusterRuleGroup.Instance = instance
						clusterRuleGroup.GroupName = alarmInfo.GetGroupName(instance.ID)
						clusterRuleGroup.Rules = make([]db2.ClusterRuleItem, 0)
					}
					clusterRuleGroup.Rules = append(clusterRuleGroup.Rules, db2.ClusterRuleItem{
						RuleName: ruleName,
						Content:  alertRule,
					})
					clusterRuleGroups[instance.GetRuleStoreKey()] = clusterRuleGroup
				} else if instance.RuleStoreType == db2.RuleStoreTypeFile || instance.RuleStoreType == db2.RuleStoreTypeK8sConfigMap {
					if err = i.PrometheusRuleCreateOrUpdate(instance, alarmInfo.GetGroupName(instance.ID), ruleName, alertRule); err != nil {
						elog.Error("alert", elog.String("step", "prometheus rule delete failed"), elog.String("err", err.Error()))
						return
					}
				}
			}
		} else if alarmInfo.Tid > 0 {
			table, _ := db2.TableInfo(invoker.Db, alarmInfo.Tid)
			instance, _ := db2.InstanceInfo(invoker.Db, table.Database.Iid)
			if instance.RuleStoreType == db2.RuleStoreTypeK8sOperator {
				clusterRuleGroup := db2.ClusterRuleGroup{}
				if tmp, ok := clusterRuleGroups[instance.GetRuleStoreKey()]; ok {
					clusterRuleGroup = tmp
				} else {
					clusterRuleGroup.ClusterId = instance.K8sClusterId
					clusterRuleGroup.Instance = instance
					clusterRuleGroup.GroupName = alarmInfo.GetGroupName(instance.ID)
					clusterRuleGroup.Rules = make([]db2.ClusterRuleItem, 0)
				}
				clusterRuleGroup.Rules = append(clusterRuleGroup.Rules, db2.ClusterRuleItem{
					RuleName: alarmInfo.GetGroupName(instance.ID),
					Content:  alarmInfo.RuleName(0),
				})
				clusterRuleGroups[instance.GetRuleStoreKey()] = clusterRuleGroup
			} else if instance.RuleStoreType == db2.RuleStoreTypeFile || instance.RuleStoreType == db2.RuleStoreTypeK8sConfigMap {
				if err = i.PrometheusRuleCreateOrUpdate(instance, alarmInfo.GetGroupName(instance.ID), alarmInfo.RuleName(0), alarmInfo.AlertRule); err != nil {
					elog.Error("alert", elog.String("step", "prometheus rule delete failed"), elog.String("err", err.Error()))
					return
				}
			}
		}
	}
	if len(clusterRuleGroups) > 0 {
		if err = i.PrometheusRuleBatchSet(clusterRuleGroups); err != nil {
			return
		}
	}
	_ = db2.AlarmFilterUpdateStatus(invoker.Db, id, map[string]interface{}{"status": db2.AlarmStatusNormal})
	if err = db2.AlarmUpdate(invoker.Db, id, map[string]interface{}{"status": db2.AlarmStatusRuleCheck}); err != nil {
		return
	}
	return
}

func (i *alert) Update(uid, alarmId int, req view.ReqAlarmCreate) (err error) {
	if req.Name == "" || req.Interval == 0 || len(req.ChannelIds) == 0 {
		return errors.New("error params")
	}
	tx := invoker.Db.Begin()
	ups := make(map[string]interface{}, 0)
	ups["name"] = req.Name
	ups["desc"] = req.Desc
	ups["interval"] = req.Interval
	ups["unit"] = req.Unit
	ups["uid"] = uid
	ups["no_data_op"] = req.NoDataOp
	ups["level"] = req.Level
	ups["channel_ids"] = db2.Ints(req.ChannelIds)
	ups["duty_officers"] = db2.Ints(req.DutyOfficers)
	ups["is_disable_resolve"] = req.IsDisableResolve
	tableIds := db2.Ints{}
	for _, f := range req.Filters {
		tableIds = append(tableIds, f.Tid)
	}
	ups["table_ids"] = tableIds
	if err = db2.AlarmUpdate(tx, alarmId, ups); err != nil {
		tx.Rollback()
		return
	}
	// filter
	if err = db2.AlarmFilterDeleteBatch(tx, alarmId); err != nil {
		tx.Rollback()
		return
	}
	// condition
	if err = db2.AlarmConditionDeleteBatch(tx, alarmId); err != nil {
		tx.Rollback()
		return
	}
	obj, err := db2.AlarmInfo(tx, alarmId)
	if err != nil {
		tx.Rollback()
		return
	}
	if err = tx.Commit().Error; err != nil {
		return
	}
	if err = i.CreateOrUpdate(&obj, req); err != nil {
		return
	}
	return
}

func (i *alert) AddPrometheusReloadChan() {
	// 10 times
	for k := 0; k < reloadTimes; k++ {
		if len(i.reloadChan) < reloadTimes {
			i.reloadChan <- time.Now().Unix()
		}
	}
}

func (i *alert) PlusOnePrometheusReloadChan() {
	// 10 times
	if len(i.reloadChan) < reloadTimes {
		i.reloadChan <- time.Now().Unix()
	}
}

func (i *alert) IsAllClosed(iid int) (err error) {
	tables, err := db2.TableListByInstanceId(invoker.Db, iid)
	if err != nil {
		return err
	}
	tidArr := make([]int, 0)
	for _, table := range tables {
		tidArr = append(tidArr, table.ID)
	}
	// Detect whether there is an alert in effect.
	conds := egorm.Conds{}
	conds["status"] = egorm.Cond{
		Op:  ">",
		Val: 1,
	}
	alarms, err := db2.AlarmListByTidArr(conds, tidArr)
	if err != nil {
		return err
	}
	if len(alarms) == 0 {
		return nil
	}
	errReason := ""
	for _, a := range alarms {
		errReason = fmt.Sprintf("%sid: %d, name: %s ;", errReason, a.ID, a.Name)
	}
	return errors.New("Contains non-closed alert:" + errReason)
}

func AllPrometheusReload() {
	instances, err := db2.InstanceList(egorm.Conds{})
	if err != nil {
		elog.Error("AllPrometheusReload", elog.String("step", "InstanceList"), elog.String("error", err.Error()))
		return
	}
	pm := make(map[string]interface{})
	for _, ins := range instances {
		if ins.PrometheusTarget != "" {
			pm[ins.PrometheusTarget] = struct{}{}
		}
	}
	for target := range pm {
		errReload := Alert.PrometheusReload(target)
		if errReload != nil {
			elog.Error("AllPrometheusReload", elog.String("step", "PrometheusReload"), elog.String("error", errReload.Error()))
		}
	}
}

// AlertRuleCheck Detect alert rules in progress
func AlertRuleCheck() error {
	conds := egorm.Conds{}
	conds["status"] = db2.AlarmStatusRuleCheck
	alarms, err := db2.AlarmList(conds)
	if err != nil {
		return err
	}
	// Find all instances
	promPool := make(map[int]*alertcomponent.Prometheus)
	for _, alarm := range alarms {
		isRuleOk := true
		rulesCheckMap := make(map[int][]string, 0)
		rulesV2 := alarm.RuleNameMap()
		if alarm.AlertRule == "" && len(rulesV2) == 0 {
			// v1版本规则已删除，v2版本规则未下发
			// 理论上这是一种异常情况
			isRuleOk = false
		}
		if len(rulesV2) > 0 {
			// v2 check
			rulesCheckMap = rulesV2
		} else if alarm.AlertRule != "" {
			// v1 check
			tableInfo, _ := db2.TableInfo(invoker.Db, alarm.Tid)
			if tableInfo.ID != 0 {
				rulesCheckMap[tableInfo.Database.Iid] = append(rulesCheckMap[tableInfo.Database.Iid], alarm.AlertRule)
			}
		}
		for iid, ruleList := range rulesCheckMap {
			prom, ok := promPool[iid]
			if !ok {
				// Cache once
				ins, _ := db2.InstanceInfo(invoker.Db, iid)
				if ins.RuleStoreType == 0 {
					isRuleOk = false
					break
				}
				prom, err = alertcomponent.NewPrometheus(ins.PrometheusTarget, ins.RuleStoreType)
				if err != nil {
					core.LoggerError("ruleCheck", "prometheus", err)
					isRuleOk = false
					break
				}
				promPool[iid] = prom
			}
			if okIsEffect, errIsEffect := prom.IsRuleTakeEffect(ruleList); errIsEffect != nil {
				core.LoggerError("ruleCheck", "isRuleTakeEffect", errIsEffect)
				isRuleOk = false
				break
			} else if !okIsEffect {
				isRuleOk = false
				break
			}
		}
		if isRuleOk {
			if err = db2.AlarmUpdate(invoker.Db, alarm.ID, map[string]interface{}{"status": db2.AlarmStatusNormal}); err != nil {
				core.LoggerError("ruleCheck", "isRuleTakeEffect", err)
				continue
			}
		}
	}
	return nil
}

func SendTestToChannel(c *db2.AlarmChannel) (err error) {
	ci, err := pusher.GetPusher(c.Typ)
	if err != nil {
		return errors.Wrapf(err, "channel type %d not found", c.Typ)
	}
	err = ci.Send(c, &db2.PushMsg{
		Title:   "Hello",
		Text:    "test/alert/alarm/告警 the availability of the alarm channel",
		Mobiles: econf.GetStringSlice("app.mobiles"),
	})
	if err != nil {
		return errors.Wrapf(err, "channel type %d send failed", c.Typ)
	}
	return
}

func AlarmAttachInfo(respList []*db2.Alarm) []view.RespAlarmList {
	res := make([]view.RespAlarmList, 0)
	cache := make(map[int]*db2.RespAlarmListRelatedInfo, 0)
	for _, a := range respList {
		alarmInfo, relatedList, errAlarmInfo := db2.GetAlarmTableInstanceInfoWithCache(a.ID, cache)
		if errAlarmInfo != nil {
			core.LoggerError("alert", "attach", errAlarmInfo)
			continue
		}
		if alarmInfo.User == nil || alarmInfo.User.ID == 0 {
			u, _ := db2.UserInfo(alarmInfo.Uid)
			alarmInfo.User = &u
		}
		alarmInfo.User.Password = "*"
		var (
			tableInfo    db2.BaseTable
			instanceInfo db2.BaseInstance
		)
		if len(relatedList) > 0 {
			tableInfo = relatedList[0].Table
			instanceInfo = relatedList[0].Instance
		}
		res = append(res, view.RespAlarmList{
			Alarm:        &alarmInfo,
			RelatedList:  relatedList,
			TableName:    tableInfo.Name,
			TableDesc:    tableInfo.Desc,
			Tid:          tableInfo.ID,
			DatabaseName: tableInfo.Database.Name,
			DatabaseDesc: tableInfo.Database.Desc,
			Did:          tableInfo.Did,
			InstanceName: instanceInfo.Name,
			InstanceDesc: instanceInfo.Desc,
			Iid:          instanceInfo.ID,
		})
	}
	return res
}

func aggregationOp(mode int, exp string, expVal string) string {
	switch mode {
	case db2.AlarmModeAggregation:
		return fmt.Sprintf("%s and %s!=-1", exp, expVal)
	default:
		return exp
	}
}

func noDataOp(op int, exp, expVal string) string {
	switch op {
	case NoDataOpDefault:
		return exp
	case NoDataOpOK:
		return fmt.Sprintf("(%s) or absent(%s)!=1", exp, expVal)
	case NoDataOpAlert:
		return fmt.Sprintf("(%s) or absent(%s)==1", exp, expVal)
	default:
		return exp
	}
}

func alarmRuleDelete(instance *db2.BaseInstance, groupName, ruleName string) (err error) {
	rc, err := rule.GetComponent(instance.RuleStoreType, &rule.Params{
		InstanceID:         instance.ID,
		RulePath:           instance.FilePath,
		ClusterId:          instance.K8sClusterId,
		Namespace:          instance.K8sNamespace,
		Configmap:          instance.K8sConfigmap,
		PrometheusOperator: instance.ConfigPrometheusOperator,
	})
	if err != nil {
		return err
	}
	return rc.Delete(groupName, ruleName)
}
