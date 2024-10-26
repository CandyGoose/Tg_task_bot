package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	tgbotapi "github.com/skinass/telegram-bot-api/v5"
)

var (
	WebhookURL string
	BotToken   string
	Port       string
)

const (
	msgNoTasks            = "Нет задач"
	msgEnterTaskName      = "Пожалуйста, введите название задачи"
	msgInvalidCommand     = "Некорректная команда"
	msgInvalidTaskNumber  = "Некорректный номер задачи"
	msgTaskNotExist       = "Задачи с таким номером не существует"
	msgTaskNotYours       = "Задача не на вас"
	msgAccepted           = "Принято"
	msgNoAssignedTasks    = "У вас нет назначенных задач."
	msgNoOwnedTasks       = "Вы не создавали задачи."
	msgAvailableCommands  = "Доступные команды:\n/tasks\n/new XXX\n/assign_*\n/unassign_*\n/resolve_*\n/my\n/owner"
	msgTaskCreated        = "Задача \"%s\" создана, id=%d"
	msgTaskAssigned       = "Задача \"%s\" назначена на вас"
	msgTaskUnassigned     = "Задача \"%s\" осталась без исполнителя"
	msgTaskResolved       = "Задача \"%s\" выполнена"
	msgTaskAssignedToUser = "Задача \"%s\" назначена на @%s"
	msgTaskResolvedByUser = "Задача \"%s\" выполнена @%s"
)

type Task struct {
	ID           int64
	Description  string
	AuthorID     int64
	AuthorName   string
	AssigneeID   int64
	AssigneeName string
	IsAssigned   bool
}

type Storage struct {
	tasksByID map[int64]*Task
	mu        sync.RWMutex
	nextID    int64
}

func NewStorage() *Storage {
	return &Storage{
		tasksByID: make(map[int64]*Task),
		nextID:    1,
	}
}

func (s *Storage) AddTask(description string, authorID int64, authorName string) *Task {
	id := atomic.AddInt64(&s.nextID, 1) - 1
	task := &Task{
		ID:          id,
		Description: description,
		AuthorID:    authorID,
		AuthorName:  authorName,
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tasksByID[id] = task
	return task
}

func (s *Storage) GetAllTasks() []*Task {
	s.mu.RLock()
	defer s.mu.RUnlock()
	tasks := make([]*Task, 0, len(s.tasksByID))
	for _, task := range s.tasksByID {
		tasks = append(tasks, task)
	}
	return tasks
}

func (s *Storage) GetTaskByID(id int64) (*Task, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	task, exists := s.tasksByID[id]
	return task, exists
}

func (s *Storage) RemoveTask(id int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.tasksByID, id)
}

type Bot struct {
	api     *tgbotapi.BotAPI
	storage *Storage
}

func NewBot(token string) (*Bot, error) {
	api, err := tgbotapi.NewBotAPI(token)
	if err != nil {
		return nil, fmt.Errorf("ошибка создания бота: %w", err)
	}
	return &Bot{
		api:     api,
		storage: NewStorage(),
	}, nil
}

func (b *Bot) SetupWebhook(url string) error {
	wh, err := tgbotapi.NewWebhook(url)
	if err != nil {
		return fmt.Errorf("ошибка установки вебхука: %w", err)
	}

	_, err = b.api.Request(wh)
	if err != nil {
		return fmt.Errorf("ошибка регистрации вебхука: %w", err)
	}
	log.Printf("Webhook установлен: %s", url)
	return nil
}

func (b *Bot) SendMessage(chatID int64, text string) error {
	msg := tgbotapi.NewMessage(chatID, text)
	_, err := b.api.Send(msg)
	return err
}

var (
	assignRegexp   = regexp.MustCompile(`^assign_(\d+)$`)
	unassignRegexp = regexp.MustCompile(`^unassign_(\d+)$`)
	resolveRegexp  = regexp.MustCompile(`^resolve_(\d+)$`)
)

func (b *Bot) handleTaskCommand(update tgbotapi.Update) error {
	tasks := b.storage.GetAllTasks()
	if len(tasks) == 0 {
		return b.SendMessage(update.Message.Chat.ID, msgNoTasks)
	}

	var response strings.Builder
	for _, task := range tasks {
		response.WriteString(fmt.Sprintf("%d. %s by @%s\n", task.ID, task.Description, task.AuthorName))
		switch {
		case !task.IsAssigned:
			response.WriteString(fmt.Sprintf("/assign_%d\n\n", task.ID))
		case task.AssigneeID == update.Message.From.ID:
			response.WriteString("assignee: я\n")
			response.WriteString(fmt.Sprintf("/unassign_%d /resolve_%d\n\n", task.ID, task.ID))
		default:
			response.WriteString(fmt.Sprintf("assignee: @%s\n\n", task.AssigneeName))
		}
	}
	return b.SendMessage(update.Message.Chat.ID, strings.TrimSpace(response.String()))
}

func (b *Bot) handleNewCommand(update tgbotapi.Update) error {
	args := update.Message.CommandArguments()
	if strings.TrimSpace(args) == "" {
		return b.SendMessage(update.Message.Chat.ID, msgEnterTaskName)
	}

	task := b.storage.AddTask(args, update.Message.From.ID, update.Message.From.UserName)
	return b.SendMessage(update.Message.Chat.ID, fmt.Sprintf(msgTaskCreated, args, task.ID))
}

func (b *Bot) handleAssignCommand(update tgbotapi.Update, cmd string) error {
	matches := assignRegexp.FindStringSubmatch(cmd)
	if len(matches) < 2 {
		return b.SendMessage(update.Message.Chat.ID, msgInvalidCommand)
	}

	taskID, err := strconv.ParseInt(matches[1], 10, 64)
	if err != nil {
		return b.SendMessage(update.Message.Chat.ID, msgInvalidTaskNumber)
	}

	task, exists := b.storage.GetTaskByID(taskID)
	if !exists {
		return b.SendMessage(update.Message.Chat.ID, msgTaskNotExist)
	}

	prevAssigneeID := task.AssigneeID
	prevAssigneeName := task.AssigneeName

	b.storage.mu.Lock()
	task.AssigneeID = update.Message.From.ID
	task.AssigneeName = update.Message.From.UserName
	task.IsAssigned = true
	b.storage.mu.Unlock()

	log.Printf("Задача \"%s\" назначена на @%s (ID: %d)", task.Description, task.AssigneeName, task.AssigneeID)

	if err := b.SendMessage(update.Message.Chat.ID, fmt.Sprintf(msgTaskAssigned, task.Description)); err != nil {
		return err
	}

	if prevAssigneeID != 0 && prevAssigneeID != update.Message.From.ID {
		log.Printf("Уведомление предыдущему исполнителю @%s", prevAssigneeName)
		if err := b.SendMessage(prevAssigneeID, fmt.Sprintf(msgTaskAssignedToUser, task.Description, task.AssigneeName)); err != nil {
			return err
		}
	}

	if task.AuthorID == update.Message.From.ID || prevAssigneeID == task.AuthorID {
		return nil
	}

	return b.SendMessage(task.AuthorID, fmt.Sprintf(msgTaskAssignedToUser, task.Description, task.AssigneeName))
}

func (b *Bot) handleUnassignCommand(update tgbotapi.Update, cmd string) error {
	matches := unassignRegexp.FindStringSubmatch(cmd)
	if len(matches) < 2 {
		return b.SendMessage(update.Message.Chat.ID, msgInvalidCommand)
	}

	taskID, err := strconv.ParseInt(matches[1], 10, 64)
	if err != nil {
		return b.SendMessage(update.Message.Chat.ID, msgInvalidTaskNumber)
	}

	task, exists := b.storage.GetTaskByID(taskID)
	if !exists {
		return b.SendMessage(update.Message.Chat.ID, msgTaskNotExist)
	}

	if task.AssigneeID != update.Message.From.ID {
		return b.SendMessage(update.Message.Chat.ID, msgTaskNotYours)
	}

	log.Printf("До снятия исполнителя: %+v", task)

	b.storage.mu.Lock()
	task.AssigneeID = 0
	task.AssigneeName = ""
	task.IsAssigned = false
	b.storage.mu.Unlock()

	log.Printf("После снятия исполнителя: %+v", task)

	if err := b.SendMessage(update.Message.Chat.ID, msgAccepted); err != nil {
		return err
	}

	// Уведомляем автора задачи, если он другой
	if task.AuthorID == update.Message.From.ID {
		return nil
	}
	return b.SendMessage(update.Message.Chat.ID, fmt.Sprintf(msgTaskUnassigned, task.Description))
}

func (b *Bot) handleResolveCommand(update tgbotapi.Update, cmd string) error {
	matches := resolveRegexp.FindStringSubmatch(cmd)
	if len(matches) < 2 {
		return b.SendMessage(update.Message.Chat.ID, msgInvalidCommand)
	}

	taskID, err := strconv.ParseInt(matches[1], 10, 64)
	if err != nil {
		return b.SendMessage(update.Message.Chat.ID, msgInvalidTaskNumber)
	}

	task, exists := b.storage.GetTaskByID(taskID)
	if !exists {
		return b.SendMessage(update.Message.Chat.ID, msgTaskNotExist)
	}

	if task.AssigneeID != update.Message.From.ID {
		return b.SendMessage(update.Message.Chat.ID, "Задача назначена не на вас")
	}

	b.storage.RemoveTask(taskID)

	log.Printf("Задача \"%s\" выполнена @%s", task.Description, task.AssigneeName)

	if err := b.SendMessage(update.Message.Chat.ID, fmt.Sprintf(msgTaskResolved, task.Description)); err != nil {
		return err
	}

	if task.AuthorID == update.Message.From.ID {
		return nil
	}
	return b.SendMessage(update.Message.Chat.ID, fmt.Sprintf(msgTaskResolvedByUser, task.Description, task.AssigneeName))
}

func (b *Bot) handleMyCommand(update tgbotapi.Update) error {
	b.storage.mu.RLock()

	var response strings.Builder
	for _, task := range b.storage.tasksByID {
		if task.AssigneeID == update.Message.From.ID {
			response.WriteString(fmt.Sprintf("%d. %s by @%s\n/unassign_%d /resolve_%d\n\n", task.ID, task.Description, task.AuthorName, task.ID, task.ID))
		}
	}

	b.storage.mu.RUnlock()

	if response.Len() == 0 {
		response.WriteString(msgNoAssignedTasks)
	}

	return b.SendMessage(update.Message.Chat.ID, strings.TrimSpace(response.String()))
}

func (b *Bot) handleOwnerCommand(update tgbotapi.Update) error {
	b.storage.mu.RLock()

	var response strings.Builder
	for _, task := range b.storage.tasksByID {
		switch {
		case task.AuthorID == update.Message.From.ID && task.IsAssigned:
			response.WriteString(fmt.Sprintf("%d. %s by @%s\n", task.ID, task.Description, task.AuthorName))
			response.WriteString(fmt.Sprintf("/unassign_%d /resolve_%d\n\n", task.ID, task.ID))
		case task.AuthorID == update.Message.From.ID:
			response.WriteString(fmt.Sprintf("%d. %s by @%s\n", task.ID, task.Description, task.AuthorName))
			response.WriteString(fmt.Sprintf("/assign_%d\n\n", task.ID))
		}
	}

	b.storage.mu.RUnlock()

	if response.Len() == 0 {
		response.WriteString(msgNoOwnedTasks)
	}

	return b.SendMessage(update.Message.Chat.ID, strings.TrimSpace(response.String()))
}

func (b *Bot) handleDefaultCommand(update tgbotapi.Update) error {
	return b.SendMessage(update.Message.Chat.ID, msgAvailableCommands)
}

func (b *Bot) handleUpdate(update tgbotapi.Update) error {
	if update.Message == nil {
		log.Println("Получено пустое сообщение, пропускаем...")
		return nil
	}

	log.Printf("Получено сообщение: %s", update.Message.Text)
	cmd := update.Message.Command()

	switch {
	case cmd == "tasks":
		return b.handleTaskCommand(update)
	case cmd == "new":
		return b.handleNewCommand(update)
	case assignRegexp.MatchString(cmd):
		return b.handleAssignCommand(update, cmd)
	case unassignRegexp.MatchString(cmd):
		return b.handleUnassignCommand(update, cmd)
	case resolveRegexp.MatchString(cmd):
		return b.handleResolveCommand(update, cmd)
	case cmd == "my":
		return b.handleMyCommand(update)
	case cmd == "owner":
		return b.handleOwnerCommand(update)
	default:
		return b.handleDefaultCommand(update)
	}
}

func init() {
	WebhookURL = os.Getenv("WEBHOOK_URL")
	BotToken = os.Getenv("BOT_TOKEN")
	Port = os.Getenv("PORT")
	if Port == "" {
		Port = "8080"
	}
}

func startTaskBot(ctx context.Context) error {
	bot, err := NewBot(BotToken)
	if err != nil {
		return err
	}

	if err = bot.SetupWebhook(WebhookURL); err != nil {
		return err
	}

	updates := bot.api.ListenForWebhook("/")

	go func() {
		server := &http.Server{Addr: ":" + Port}
		log.Println("Запуск локального HTTP сервера на порту:", Port)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("HTTP сервер завершился с ошибкой: %v", err)
		}
	}()

	log.Println("Вебхук успешно настроен, бот готов получать обновления")

	for {
		select {
		case <-ctx.Done():
			log.Println("Завершение работы бота...")
			return nil
		case update := <-updates:
			if err := bot.handleUpdate(update); err != nil {
				log.Printf("Ошибка обработки команды: %s", err)
			}
		}
	}
}

func run() error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := startTaskBot(ctx); err != nil {
		return err
	}

	return nil
}

func main() {
	if err := run(); err != nil {
		log.Println("Ошибка при запуске бота:", err)
	}
}
