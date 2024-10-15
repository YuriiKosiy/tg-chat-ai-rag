package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
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
	//OpenAIModel = os.Getenv("OPENAI_MODEL") // Модель OpenAI
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

		// Меню для бота
		//menu := &telebot.ReplyMarkup{
		//	ReplyKeyboard: [][]telebot.ReplyButton{
		//		{{Text: "Ask на основі векторної бази"}}, // Основна дія
		//	},
		//}

		// Обробка команди /start
		// Форматуємо повідомлення перед відправкою у /start з використанням HTML
		aibot.Handle("/start", func(m telebot.Context) error {
			log.Printf("Користувач ID %d почав сесію.", m.Sender().ID)

			// Форматуємо повідомлення перед відправкою
			msg := fmt.Sprintf("Цей чат-бот створений для надавання інформації про людину та її трудовий досвід. Версія чат-боту: %s", appVersion)

			return m.Send(msg, &telebot.SendOptions{
				ParseMode: telebot.ModeHTML, // Використовуємо HTML форматування
			})
		})

		// Обробка текстових запитів
		aibot.Handle(telebot.OnText, func(m telebot.Context) error {
			userQuery := m.Text() // Текст запиту користувача
			log.Printf("Запит користувача: %s", userQuery)

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

		aibot.Handle(telebot.OnDocument, func(m telebot.Context) error {
			file := m.Message().Document

			// Завантажуємо файл
			fileBytes, err := downloadTelegramFile(aibot, file.FileID)
			if err != nil {
				log.Printf("Помилка завантаження файлу: %v", err)
				return m.Send(fmt.Sprintf("Помилка завантаження файлу: %v", err))
			}

			// Визначаємо тип файлу (PDF або JSON)
			if isPDF(file.FileName) {
				return processAndUploadPDF(fileBytes, file.FileName, m) // Обробка PDF
			} else if isJSON(file.FileName) {
				return processAndUploadJSON(fileBytes, file.FileName, m) // Обробка JSON
			}

			return m.Send("Невідомий формат файлу. Завантажте, будь ласка, тільки PDF або JSON.")
		})

		// Старт бота
		aibot.Start()

		log.Printf("Бот успішно стартував!")
	},
}

//Функції для завантаження та векторизації

// Перевірка, чи є файл PDF
func isPDF(fileName string) bool {
	return len(fileName) > 4 && fileName[len(fileName)-4:] == ".pdf"
}

// Перевірка, чи є файл JSON
func isJSON(fileName string) bool {
	return len(fileName) > 5 && fileName[len(fileName)-5:] == ".json"
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

// Обробка та індексація PDF файлів
func processAndUploadPDF(fileBytes []byte, fileName string, m telebot.Context) error {
	// 1. Витягуємо текст з PDF файлу
	text, err := extractTextFromPDF(fileBytes)
	if err != nil {
		log.Printf("Помилка обробки PDF: %v", err)
		return m.Send("Помилка обробки PDF файла.")
	}

	// 2. Векторизуємо текст через OpenAI API
	queryEmbedding, err := getQueryEmbeddingFromOpenAI(text)
	if err != nil {
		log.Printf("Помилка векторизації PDF: %v", err)
		return m.Send("Помилка векторизації тексту з PDF.")
	}

	// 3. Додаємо вектор у Pinecone
	err = upsertVectorToPinecone(queryEmbedding, map[string]interface{}{
		"file": fileName, "text": text,
	})
	if err != nil {
		log.Printf("Помилка додавання в Pinecone: %v", err)
		return m.Send("Помилка завантаження даних у Pinecone.")
	}

	return m.Send("PDF успішно завантажено та додано до векторної бази.")
}

// Обробка та індексація JSON файлів
func processAndUploadJSON(fileBytes []byte, fileName string, m telebot.Context) error {
	var jsonData map[string]interface{}
	if err := json.Unmarshal(fileBytes, &jsonData); err != nil {
		log.Printf("Помилка обробки JSON: %v", err)
		return m.Send("Помилка обробки JSON файла.")
	}

	// Якщо є текст або інша інформація, яку потрібно векторизувати, векторизуємо її
	if text, ok := jsonData["text"].(string); ok {
		queryEmbedding, err := getQueryEmbeddingFromOpenAI(text)
		if err != nil {
			log.Printf("Помилка векторизації JSON: %v", err)
			return m.Send("Помилка векторизації тексту з JSON.")
		}

		// Додаємо вектори з метаданими JSON у Pinecone
		err = upsertVectorToPinecone(queryEmbedding, jsonData)
		if err != nil {
			log.Printf("Помилка збереження в Pinecone з JSON: %v", err)
			return m.Send("Помилка завантаження даних з JSON у Pinecone.")
		}

		return m.Send("JSON успішно завантажено та додано до векторної бази.")
	}

	return m.Send("JSON не містить текстових даних для векторизації.")
}

// Витягуємо текст з PDF
func extractTextFromPDF(fileBytes []byte) (string, error) {
	// Реалізуйте ваше витягування тексту з PDF тут
	// Можна використовувати сторонні бібліотеки для роботи з PDF, як pdfcpu або unidoc
	return "Text from PDF", nil
}

// Додавання вектора до Pinecone з метаданими
func upsertVectorToPinecone(embedding []float32, metadata map[string]interface{}) error {
	clientParams := pinecone.NewClientParams{
		ApiKey: PineconeAPIKey,
	}
	client, err := pinecone.NewClient(clientParams)
	if err != nil {
		return fmt.Errorf("Помилка створення Pinecone клієнта: %v", err)
	}

	// Деталі індексу
	indexDesc, err := client.DescribeIndex(context.Background(), PineconeIndex)
	if err != nil {
		return fmt.Errorf("Помилка опису індексу Pinecone: %v", err)
	}

	// Підключаємося до індексу
	indexConnection, err := client.Index(pinecone.NewIndexConnParams{Host: indexDesc.Host})
	if err != nil {
		return fmt.Errorf("Помилка підключення до індексу: %v", err)
	}

	// Метадані векторів у форматі JSON
	metadataStruct, err := structpb.NewStruct(metadata)
	if err != nil {
		return fmt.Errorf("Помилка перетворення метаданих: %v", err)
	}

	// Додаємо вектори і метадані в Pinecone
	_, err = indexConnection.UpsertVectors(context.Background(), []*pinecone.Vector{
		{
			Id:       fmt.Sprintf("doc-%d", time.Now().Unix()), // Унікальний ID для документа
			Values:   embedding,                                // Вектор з OpenAI
			Metadata: metadataStruct,                           // Метадані
		},
	})
	if err != nil {
		return fmt.Errorf("Запит UpsertVectors не вдався: %v", err)
	}

	return nil
}

// Отримуємо ембеддинг через OpenAI з використанням 'text-embedding-ada-002'
func getQueryEmbeddingFromOpenAI(query string) ([]float32, error) {
	client := openai.NewClient(OpenAIKey)

	embeddingReq := openai.EmbeddingRequest{
		Model: "text-embedding-ada-002", // Чітко вказуємо модель для векторизації
		Input: []string{query},
	}

	resp, err := client.CreateEmbeddings(context.Background(), embeddingReq)
	if err != nil {
		return nil, fmt.Errorf("Помилка створення ембеддингів через OpenAI: %v", err)
	}

	if len(resp.Data) == 0 {
		return nil, fmt.Errorf("OpenAI не повернув векторів.")
	}

	log.Printf("API OpenAI успішно згенерував вектор для запиту: %s", query)

	return resp.Data[0].Embedding, nil
}

// Виконуємо пошук у Pinecone за релевантними даними для запиту
func searchPinecone(embedding []float32) (*pinecone.QueryVectorsResponse, error) {
	clientParams := pinecone.NewClientParams{
		ApiKey: PineconeAPIKey,
	}
	client, err := pinecone.NewClient(clientParams)
	if err != nil {
		return nil, fmt.Errorf("Помилка створення клієнта Pinecone: %v", err)
	}

	// Отримуємо інформацію про індекс
	indexDesc, err := client.DescribeIndex(context.Background(), PineconeIndex)
	if err != nil {
		return nil, fmt.Errorf("Помилка під час опису індексу: %v", err)
	}

	// Приєднуємось до індексу через хост
	indexConnection, err := client.Index(pinecone.NewIndexConnParams{Host: indexDesc.Host})
	if err != nil {
		return nil, fmt.Errorf("Помилка підключення до індексу: %v", err)
	}

	// Створюємо запит на основі векторного представлення
	queryRequest := &pinecone.QueryByVectorValuesRequest{
		Vector:          embedding,
		TopK:            5,    // Повернути 5 найбільш релевантних записів.
		IncludeValues:   true, // Додаємо значення векторів.
		IncludeMetadata: true, // Важливо отримати метадані.
	}

	// Запит до Pinecone
	response, err := indexConnection.QueryByVectorValues(context.Background(), queryRequest)
	if err != nil {
		return nil, fmt.Errorf("Помилка запиту до Pinecone: %v", err)
	}

	log.Printf("Запит до Pinecone був успішним. Знайдено збігів: %d", len(response.Matches))

	return response, nil
}

// **Формування відповіді через OpenAI GPT-4**
// Генерація відповіді з використанням всіх знайдених релевантних даних через GPT-4
// Генерація відповіді з використанням GPT-4
func generateFinalAnswerFromOpenAI(query string, matches *pinecone.QueryVectorsResponse) (string, error) {

	// Створення OpenAI клієнта
	client := openai.NewClient(OpenAIKey)

	// Підготовка результатів для GPT-4
	var resultsDescription string
	for _, match := range matches.Matches {
		vectorID := match.Vector.Id
		values, _ := json.Marshal(match.Vector.Values)

		// Метадані
		metadata := "Метадані відсутні"
		if match.Vector.Metadata != nil {
			metadataMap := match.Vector.Metadata.AsMap()
			metadataBytes, _ := json.Marshal(metadataMap)
			metadata = string(metadataBytes)
		}

		// Опис результату для GPT-4
		resultsDescription += fmt.Sprintf("ID: %s, Векторні значення: %s, Метадані: %s. Оцінка релевантності: %f\n", vectorID, values, metadata, match.Score)

		// Обмеження обсягу для GPT
		if len(resultsDescription) > 100000000 {
			resultsDescription += "\n(Деякі записи були виключені через обмеження обсягу)."
			break
		}
	}

	log.Printf("Формування результатів з Pinecone для GPT-4")

	// Запит до GPT-4 із контекстом запиту користувача
	chatRequest := openai.ChatCompletionRequest{
		Model: OpenAIModel, // Модель OpenAI з змінної середовища
		Messages: []openai.ChatCompletionMessage{
			{
				Role:    openai.ChatMessageRoleSystem,
				Content: "Ти чат-асистент, який відповідає на основі даних з векторної бази Pinecone. Всі відповіді мають базуватися на знайденій інформації. Якщо знайдено кілька варіантів, надай зведення з кожного.",
			},
			{
				Role:    openai.ChatMessageRoleUser,
				Content: fmt.Sprintf("Ось ваш запит: %s. Ось знайдені дані через Pinecone: %s", query, resultsDescription),
			},
		},
	}

	// Надсилаємо запит до GPT-4
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
