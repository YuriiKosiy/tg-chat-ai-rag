package cmd

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	pinecone "github.com/pinecone-io/go-pinecone/pinecone"
	openai "github.com/sashabaranov/go-openai"
	"github.com/spf13/cobra"
	"google.golang.org/protobuf/types/known/structpb"
	telebot "gopkg.in/telebot.v3"
)

// Користувацька сесія для відстеження стану
type UserSession struct {
	AwaitingDocument bool
}

var (
	// Сесії користувачів для відстеження дій
	userSessions = struct {
		sync.RWMutex
		sessions map[int64]*UserSession
	}{sessions: make(map[int64]*UserSession)}

	// Через змінні оточення беремо ключі для Telegram, OpenAI та Pinecone
	TelegramToken  = os.Getenv("TELE_TOKEN")       // Telegram токен
	OpenAIKey      = os.Getenv("OPENAI_API_KEY")   // OpenAI API Key
	PineconeAPIKey = os.Getenv("PINECONE_API_KEY") // Pinecone API Key

	// Pinecone спеціфічні налаштування:
	PineconeIndex = "telegram"  // Назва індексу
	PineconeEnv   = "us-east-1" // Середовище Pinecone (регіон)

	// Винесення OpenAI моделі до змінних середовища
	OpenAIModel = "gpt-4o"
)

// Налаштування команди для Cobra
var aibotCmd = &cobra.Command{
	Use:   "aibot",
	Short: "Telegram бот з інтеграцією з Pinecone та GPT-4.",
	Run: func(cmd *cobra.Command, args []string) {
		log.Printf("AI бот запущено! Версія: %s", appVersion)

		// Перевіряємо змінні середовища
		if TelegramToken == "" || OpenAIKey == "" || PineconeAPIKey == "" || PineconeEnv == "" || OpenAIModel == "" {
			log.Fatalf("Відсутні необхідні змінні середовища.")
		}

		// Ініціалізація Telegram-бота
		aibot, err := telebot.NewBot(telebot.Settings{
			Token:  TelegramToken,
			Poller: &telebot.LongPoller{Timeout: 10 * time.Second},
		})

		if err != nil {
			log.Fatalf("Не вдалося запустити бота: %v", err)
		}

		// Обробка команди /start
		aibot.Handle("/start", func(m telebot.Context) error {
			log.Printf("Користувач ID %d почав сесію.", m.Sender().ID)

			msg := fmt.Sprintf("Цей чат-бот створений для надавання інформації про людину та її трудовий досвід. Версія чат-боту: %s", appVersion)

			return m.Send(msg, &telebot.SendOptions{
				ParseMode: telebot.ModeHTML,
			})
		})

		// Обробка текстових запитів як чат
		aibot.Handle(telebot.OnText, func(m telebot.Context) error {
			userQuery := m.Text()
			log.Printf("Запит користувача: %s", userQuery)

			// Якщо текст - URL, обробляємо його як файл
			if isURL(userQuery) {
				log.Printf("Отримано посилання на файл: %s", userQuery)

				// Завантажуємо файл з URL
				fileBytes, err := downloadFileFromURL(userQuery)
				if err != nil {
					log.Printf("Помилка завантаження файлу з посилання: %v", err)
					return m.Send(fmt.Sprintf("Помилка завантаження файлу: %v", err))
				}

				// Визначення типу файлу за посиланням (XML, PDF або JSON)
				if isXML(userQuery) {
					return processAndUploadXML(fileBytes, "external_file.xml", m) // XML
				} else if isJSON(userQuery) {
					return processAndUploadJSON(fileBytes, "external_file.json", m) // JSON
				} else if isPDF(userQuery) {
					return processAndUploadPDF(fileBytes, "external_file.pdf", m) // PDF
				}

				return m.Send("Невідомий формат файлу. Підтримуються тільки XML, JSON або PDF.")
			}

			// Якщо це текстовий запит, продовжуємо як звичайний чат
			if userQuery == "" {
				return m.Send("Будь ласка, введіть запит.")
			}

			// 1. Векторизуємо запит через OpenAI
			queryEmbedding, err := getQueryEmbeddingFromOpenAI(userQuery)
			if err != nil {
				log.Printf("Помилка у OpenAI: %v", err)
				return m.Send(fmt.Sprintf("Помилка у генерації вектору через OpenAI: %v", err))
			}

			// 2. Пошук у Pinecone
			matches, err := searchPinecone(queryEmbedding)
			if err != nil || len(matches.Matches) == 0 {
				log.Printf("Pinecone не повернув релевантної інформації або виникла проблема із запитом: %v", err)
				return m.Send("Не знайдено релевантних збігів у Pinecone.")
			}

			// 3. Генерація відповіді GPT-4 із обмеженим контекстом
			answer, err := generateFinalAnswerFromOpenAI(userQuery, matches)
			if err != nil {
				log.Printf("Помилка під час спроби згенерувати відповідь через GPT-4: %v", err)
				return m.Send(fmt.Sprintf("GPT-4 не зміг згенерувати відповідь: %v", err))
			}

			log.Printf("Повернена відповідь від ChatGPT: %s", answer)

			// Повернення результату користувачеві
			return m.Send(fmt.Sprintf("%s", answer))
		})

		// Обробка файлів (PDF, JSON, XML)
		aibot.Handle(telebot.OnDocument, func(m telebot.Context) error {
			file := m.Message().Document

			// Завантажуємо файл
			fileBytes, err := downloadTelegramFile(aibot, file.FileID)
			if err != nil {
				log.Printf("Помилка завантаження файлу: %v", err)
				return m.Send(fmt.Sprintf("Помилка завантаження файлу: %v", err))
			}

			// Визначення типу файлу (PDF, JSON або XML)
			if isPDF(file.FileName) {
				return processAndUploadPDF(fileBytes, file.FileName, m) // PDF
			} else if isJSON(file.FileName) {
				return processAndUploadJSON(fileBytes, file.FileName, m) // JSON
			} else if isXML(file.FileName) {
				return processAndUploadXML(fileBytes, file.FileName, m) // XML
			}

			return m.Send("Невідомий формат файлу. Завантажте, будь ласка, тільки PDF, JSON або XML.")
		})

		// Запускаємо бота
		aibot.Start()

		log.Printf("Бот успішно стартував!")
	},
}

// Завантажуємо файл з URL
func downloadFileFromURL(url string) ([]byte, error) {
	resp, err := http.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	// Перевірка відповіді
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("помилка завантаження файлу: %d", resp.StatusCode)
	}

	return io.ReadAll(resp.Body)
}

// Завантажуємо файл з Telegram
func downloadTelegramFile(bot *telebot.Bot, fileID string) ([]byte, error) {
	file, err := bot.FileByID(fileID)
	if err != nil {
		return nil, err
	}

	resp, err := http.Get(fmt.Sprintf("https://api.telegram.org/file/bot%s/%s", TelegramToken, file.FilePath))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	return io.ReadAll(resp.Body)
}

// Перевірка чи є рядок URL
func isURL(input string) bool {
	return strings.HasPrefix(input, "http://") || strings.HasPrefix(input, "https://")
}

// Перевірка, чи є файл PDF
func isPDF(fileName string) bool {
	return len(fileName) > 4 && fileName[len(fileName)-4:] == ".pdf"
}

// Перевірка, чи є файл JSON
func isJSON(fileName string) bool {
	return len(fileName) > 5 && fileName[len(fileName)-5:] == ".json"
}

// Перевірка, чи є файл XML
func isXML(fileName string) bool {
	return len(fileName) > 4 && fileName[len(fileName)-4:] == ".xml"
}

// Обробка та індексація XML файлів
func processAndUploadXML(fileBytes []byte, fileName string, m telebot.Context) error {
	type Offer struct {
		Name        string `xml:"name"`
		Description string `xml:"description"`
		Price       string `xml:"price"`
	}
	type YmlCatalog struct {
		Offers []Offer `xml:"shop>offers>offer"`
	}

	var catalog YmlCatalog
	if err := xml.Unmarshal(fileBytes, &catalog); err != nil {
		log.Printf("Помилка обробки XML: %v", err)
		return m.Send("Помилка обробки XML файла.")
	}

	var textContent string
	for _, offer := range catalog.Offers {
		textContent += fmt.Sprintf("Назва: %s\nОпис: %s\nЦіна: %s\n\n", cleanText(offer.Name), cleanText(offer.Description), offer.Price)
	}

	// Векторизація через OpenAI
	queryEmbedding, err := getQueryEmbeddingFromOpenAI(textContent)
	if err != nil {
		log.Printf("Помилка векторизації XML: %v", err)
		return m.Send("Помилка векторизації тексту з XML.")
	}

	// Додавання вектору до Pinecone
	err = upsertVectorToPinecone(queryEmbedding, map[string]interface{}{
		"file": fileName, "text": textContent,
	})
	if err != nil {
		log.Printf("Помилка завантаження у Pinecone з XML: %v", err)
		return m.Send("Помилка завантаження даних у Pinecone.")
	}

	return m.Send("XML успішно завантажено та додано до векторної бази.")
}

// Очистка тексту від HTML-тегів
func cleanText(input string) string {
	re := regexp.MustCompile("<[^>]*>")
	return re.ReplaceAllString(input, "")
}

// Зберігання векторів у Pinecone
func upsertVectorToPinecone(embedding []float32, metadata map[string]interface{}) error {
	clientParams := pinecone.NewClientParams{
		ApiKey: PineconeAPIKey,
	}
	client, err := pinecone.NewClient(clientParams)
	if err != nil {
		return fmt.Errorf("Помилка створення Pinecone клієнта: %v", err)
	}

	indexConnection, err := client.Index(pinecone.NewIndexConnParams{Host: "host-url"})
	if err != nil {
		return fmt.Errorf("Помилка підключення до індексу: %v", err)
	}

	metadataStruct, err := structpb.NewStruct(metadata)
	if err != nil {
		return fmt.Errorf("Помилка перетворення метаданих: %v", err)
	}

	_, err = indexConnection.UpsertVectors(context.Background(), []*pinecone.Vector{
		{
			Id:       fmt.Sprintf("doc-%d", time.Now().Unix()),
			Values:   embedding,
			Metadata: metadataStruct,
		},
	})
	if err != nil {
		return fmt.Errorf("Запит UpsertVectors не вдався: %v", err)
	}

	return nil
}

// Векторизація через OpenAI
func getQueryEmbeddingFromOpenAI(query string) ([]float32, error) {
	client := openai.NewClient(OpenAIKey)

	embeddingReq := openai.EmbeddingRequest{
		Model: "text-embedding-ada-002",
		Input: []string{query},
	}

	resp, err := client.CreateEmbeddings(context.Background(), embeddingReq)
	if err != nil {
		return nil, fmt.Errorf("Помилка створення ембеддингів через OpenAI: %v", err)
	}

	if len(resp.Data) == 0 {
		return nil, fmt.Errorf("OpenAI не повернув векторів.")
	}

	return resp.Data[0].Embedding, nil
}

// Пошук у Pinecone
func searchPinecone(embedding []float32) (*pinecone.QueryVectorsResponse, error) {
	clientParams := pinecone.NewClientParams{
		ApiKey: PineconeAPIKey,
	}
	client, err := pinecone.NewClient(clientParams)
	if err != nil {
		return nil, fmt.Errorf("Помилка створення клієнта Pinecone: %v", err)
	}

	indexConnection, err := client.Index(pinecone.NewIndexConnParams{Host: "host-url"})
	if err != nil {
		return nil, fmt.Errorf("Помилка підключення до індексу: %v", err)
	}

	queryRequest := &pinecone.QueryByVectorValuesRequest{
		Vector:          embedding,
		TopK:            5,
		IncludeValues:   true,
		IncludeMetadata: true,
	}

	response, err := indexConnection.QueryByVectorValues(context.Background(), queryRequest)
	if err != nil {
		return nil, fmt.Errorf("Помилка запиту до Pinecone: %v", err)
	}

	return response, nil
}

// Генерація відповіді через GPT-4
func generateFinalAnswerFromOpenAI(query string, matches *pinecone.QueryVectorsResponse) (string, error) {
	client := openai.NewClient(OpenAIKey)

	var resultsDescription string
	for _, match := range matches.Matches {
		metadata := match.Vector.Metadata.AsMap()
		metadataBytes, _ := json.Marshal(metadata)

		resultsDescription += fmt.Sprintf("ID: %s\nМетадані: %s\n\n", match.Vector.Id, string(metadataBytes))
	}

	chatRequest := openai.ChatCompletionRequest{
		Model: OpenAIModel,
		Messages: []openai.ChatCompletionMessage{
			{
				Role:    openai.ChatMessageRoleSystem,
				Content: "Ти чат-асистент, який базується на векторній базі даних Pinecone. Відповіді мають ґрунтуватися на релевантній інформації.",
			},
			{
				Role:    openai.ChatMessageRoleUser,
				Content: fmt.Sprintf("Ось ваш запит: %s.\nОсь знайдені дані з Pinecone: %s", query, resultsDescription),
			},
		},
	}

	resp, err := client.CreateChatCompletion(context.Background(), chatRequest)
	if err != nil {
		return "", fmt.Errorf("GPT-4 не зміг згенерувати відповідь: %v", err)
	}

	return resp.Choices[0].Message.Content, nil
}

func init() {
	// Додаємо команду до rootCmd через Cobra
	rootCmd.AddCommand(aibotCmd)
}
