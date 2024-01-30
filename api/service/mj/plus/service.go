package plus

import (
	"chatplus/core/types"
	"chatplus/store"
	"chatplus/store/model"
	"chatplus/utils"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"gorm.io/gorm"
)

// Service MJ 绘画服务
type Service struct {
	Name             string  // service Name
	Client           *Client // MJ Client
	taskQueue        *store.RedisQueue
	notifyQueue      *store.RedisQueue
	db               *gorm.DB
	maxHandleTaskNum int32             // max task number current service can handle
	HandledTaskNum   int32             // already handled task number
	taskStartTimes   map[int]time.Time // task start time, to check if the task is timeout
	taskTimeout      int64
}

func NewService(name string, taskQueue *store.RedisQueue, notifyQueue *store.RedisQueue, maxTaskNum int32, timeout int64, db *gorm.DB, client *Client) *Service {
	return &Service{
		Name:             name,
		db:               db,
		taskQueue:        taskQueue,
		notifyQueue:      notifyQueue,
		Client:           client,
		taskTimeout:      timeout,
		maxHandleTaskNum: maxTaskNum,
		taskStartTimes:   make(map[int]time.Time, 0),
	}
}

func (s *Service) Run() {
	logger.Infof("Starting MidJourney job consumer for %s", s.Name)
	for {
		s.checkTasks()
		if !s.canHandleTask() {
			// current service is full, can not handle more task
			// waiting for running task finish
			time.Sleep(time.Second * 3)
			continue
		}

		var task types.MjTask
		err := s.taskQueue.LPop(&task)
		if err != nil {
			logger.Errorf("taking task with error: %v", err)
			continue
		}

		// if it's reference message, check if it's this channel's  message
		if task.ChannelId != "" && task.ChannelId != s.Name {
			logger.Debugf("handle other service task, name: %s, channel_id: %s, drop it.", s.Name, task.ChannelId)
			s.taskQueue.RPush(task)
			time.Sleep(time.Second)
			continue
		}

		logger.Infof("%s handle a new MidJourney task: %+v", s.Name, task)
		var res ImageRes
		switch task.Type {
		case types.TaskImage:
			res, err = s.Client.Imagine(task)
			break
		case types.TaskUpscale:
			res, err = s.Client.Upscale(task)
			break
		case types.TaskVariation:
			res, err = s.Client.Variation(task)
			break
		case types.TaskBlend:
			res, err = s.Client.Blend(task)
			break
		case types.TaskSwapFace:
			res, err = s.Client.SwapFace(task)
			break
		}

		if err != nil || (res.Code != 1 && res.Code != 22) {
			errMsg := fmt.Sprintf("%v,%s", err, res.Description)
			logger.Error("绘画任务执行失败：", errMsg)
			// update the task progress
			s.db.Model(&model.MidJourneyJob{Id: task.Id}).UpdateColumns(map[string]interface{}{
				"progress": -1,
				"err_msg":  errMsg,
			})
			// 任务失败，通知前端
			s.notifyQueue.RPush(task.UserId)
			// restore img_call quota
			if task.Type.String() != types.TaskUpscale.String() {
				s.db.Model(&model.User{}).Where("id = ?", task.UserId).UpdateColumn("img_calls", gorm.Expr("img_calls + ?", 1))
			}

			// TODO: 任务提交失败，加入队列重试
			continue
		}
		logger.Infof("任务提交成功：%+v", res)
		// lock the task until the execute timeout
		s.taskStartTimes[int(task.Id)] = time.Now()
		atomic.AddInt32(&s.HandledTaskNum, 1)
		// 更新任务 ID/频道
		s.db.Debug().Model(&model.MidJourneyJob{Id: task.Id}).UpdateColumns(map[string]interface{}{
			"task_id":    res.Result,
			"channel_id": s.Name,
		})

	}
}

// check if current service instance can handle more task
func (s *Service) canHandleTask() bool {
	handledNum := atomic.LoadInt32(&s.HandledTaskNum)
	return handledNum < s.maxHandleTaskNum
}

// remove the expired tasks
func (s *Service) checkTasks() {
	for k, t := range s.taskStartTimes {
		if time.Now().Unix()-t.Unix() > s.taskTimeout {
			delete(s.taskStartTimes, k)
			atomic.AddInt32(&s.HandledTaskNum, -1)
			// delete task from database
			s.db.Delete(&model.MidJourneyJob{Id: uint(k)}, "progress < 100")
		}
	}
}

type CBReq struct {
	Id          string      `json:"id"`
	Action      string      `json:"action"`
	Status      string      `json:"status"`
	Prompt      string      `json:"prompt"`
	PromptEn    string      `json:"promptEn"`
	Description string      `json:"description"`
	SubmitTime  int64       `json:"submitTime"`
	StartTime   int64       `json:"startTime"`
	FinishTime  int64       `json:"finishTime"`
	Progress    string      `json:"progress"`
	ImageUrl    string      `json:"imageUrl"`
	FailReason  interface{} `json:"failReason"`
	Properties  struct {
		FinalPrompt string `json:"finalPrompt"`
	} `json:"properties"`
}

func (s *Service) Notify(job model.MidJourneyJob) error {
	task, err := s.Client.QueryTask(job.TaskId)
	if err != nil {
		return err
	}

	// 任务执行失败了
	if task.FailReason != "" {
		s.db.Model(&model.MidJourneyJob{Id: job.Id}).UpdateColumns(map[string]interface{}{
			"progress": -1,
			"err_msg":  task.FailReason,
		})
		return fmt.Errorf("task failed: %v", task.FailReason)
	}

	if len(task.Buttons) > 0 {
		job.Hash = GetImageHash(task.Buttons[0].CustomId)
	}
	oldProgress := job.Progress
	job.Progress = utils.IntValue(strings.Replace(task.Progress, "%", "", 1), 0)
	job.Prompt = task.PromptEn
	if task.ImageUrl != "" {
		if s.Client.Config.CdnURL != "" {
			job.OrgURL = strings.Replace(task.ImageUrl, s.Client.Config.ApiURL, s.Client.Config.CdnURL, 1)
		} else {
			job.OrgURL = task.ImageUrl
		}
	}
	job.MessageId = task.Id
	tx := s.db.Updates(&job)
	if tx.Error != nil {
		return fmt.Errorf("error with update database: %v", tx.Error)
	}
	if task.Status == "SUCCESS" {
		// release lock task
		atomic.AddInt32(&s.HandledTaskNum, -1)
	}
	// 通知前端更新任务进度
	if oldProgress != job.Progress {
		s.notifyQueue.RPush(job.UserId)
	}
	return nil
}

func GetImageHash(action string) string {
	split := strings.Split(action, "::")
	if len(split) > 5 {
		return split[4]
	}
	return split[len(split)-1]
}
