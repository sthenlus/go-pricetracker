package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/gocolly/colly/v2"
	"github.com/robfig/cron/v3"

	"github.com/joho/godotenv"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
)

type Product struct {
	ID        uint   `gorm:"primaryKey"`
	Name      string `gorm:"unique;not null"`
	URL       string `gorm:"type:varchar(512);unique;not null"` // 512 karakter!
	Prices    []PriceHistory
	CreatedAt time.Time
	UpdatedAt time.Time
}

type PriceHistory struct {
	ID        uint    `gorm:"primaryKey"`
	ProductID uint    `gorm:"not null"`
	Price     float64 `gorm:"not null"`
	CreatedAt time.Time
}

type Target struct {
	URL           string `json:"url"`
	NameSelector  string `json:"name_selector"`
	PriceSelector string `json:"price_selector"`
}

type Config struct {
	Targets []Target `json:"targets"`
}

func scrapeProduct(target Target) (string, float64) {
	c := colly.NewCollector()

	c.UserAgent = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/125.0.0.0 Safari/537.36"

	var productName string
	var productPrice float64

	c.OnHTML(target.NameSelector, func(e *colly.HTMLElement) {
		productName = strings.TrimSpace(e.Text)
	})

	c.OnHTML(target.PriceSelector, func(e *colly.HTMLElement) {
		// Amazon'da bu seçici bazen birden fazla sonuç döndürebilir,
		// bu yüzden sadece ilk bulduğumuz ve içinde fiyat olanı alıyoruz.
		if productPrice > 0 {
			return
		}

		priceStr := strings.TrimSpace(e.Text)
		// Fiyat formatı "₺139,90" şeklindedir.
		priceStr = strings.ReplaceAll(priceStr, "₺", "")   // Para birimi simgesini kaldır
		priceStr = strings.ReplaceAll(priceStr, " TL", "") // TL simgesini kaldır (varsa)
		priceStr = strings.ReplaceAll(priceStr, ".", "")   // Binlik ayıracını kaldır (varsa)
		priceStr = strings.ReplaceAll(priceStr, ",", ".")  // Ondalık ayıracını noktaya çevir

		price, err := strconv.ParseFloat(priceStr, 64)
		if err == nil {
			productPrice = price
		}
	})

	c.OnRequest(func(r *colly.Request) {
		fmt.Printf("Ziyaret ediliyor: %s\n", r.URL)
	})

	c.Visit(target.URL)

	return productName, productPrice
}

func setupDatabase() *gorm.DB {
	err := godotenv.Load()
	if err != nil {
		log.Fatal("Hata: .env dosyası yüklenemedi")
	}
	dbUser := os.Getenv("DB_USER")
	dbPassword := os.Getenv("DB_PASSWORD")
	dbHost := os.Getenv("DB_HOST")
	dbPort := os.Getenv("DB_PORT")
	dbName := os.Getenv("DB_NAME")
	dsn := fmt.Sprintf("%s:%s@tcp(%s:%s)/%s?charset=utf8mb4&parseTime=True&loc=Local",
		dbUser, dbPassword, dbHost, dbPort, dbName)
	db, err := gorm.Open(mysql.Open(dsn), &gorm.Config{})
	if err != nil {
		log.Fatalf("Hata: Veritabanına bağlanılamadı: %v", err)
	}
	db.AutoMigrate(&Product{}, &PriceHistory{})
	return db
}

// --- Ana Kazıma İşini Yapan Fonksiyon ---
func runScrapingJob(db *gorm.DB) {
	fmt.Printf("\n--- Fiyat Takip Görevi Başlatıldı (%s) ---\n", time.Now().Format("2006-01-02 15:04:05"))

	configFile, err := os.ReadFile("config.json")
	if err != nil {
		log.Printf("Hata: config.json okunamadı: %v", err)
		return
	}

	var config Config
	json.Unmarshal(configFile, &config)

	for _, target := range config.Targets {
		name, price := scrapeProduct(target)
		if name == "" || price == 0 {
			log.Printf("Hata: Ürün bilgileri alınamadı -> %s", target.URL)
			continue
		}
		fmt.Printf("   Ürün: %s, Güncel Fiyat: %.2f TL\n", name, price)

		var product Product
		db.FirstOrCreate(&product, Product{URL: target.URL, Name: name})

		var lastPrice PriceHistory
		db.Where("product_id = ?", product.ID).Order("created_at desc").First(&lastPrice)

		// Eğer son fiyat, güncel fiyattan farklıysa VEYA bu ürün için hiç fiyat kaydı yoksa
		if lastPrice.Price != price || lastPrice.ID == 0 {
			fmt.Println("   -> Fiyat değişmiş! Yeni fiyat veritabanına kaydediliyor.")
			priceRecord := PriceHistory{
				ProductID: product.ID,
				Price:     price,
			}
			db.Create(&priceRecord)
		} else {
			fmt.Println("   -> Fiyat aynı. Kayıt atlanıyor.")
		}

	}
	fmt.Println("--- Görev Tamamlandı ---")
}

func cleanupOldPriceRecords(db *gorm.DB) {
	fmt.Printf("\n--- Eski Kayıtları Temizleme Görevi Başlatıldı (%s) ---\n", time.Now().Format("2006-01-02 15:04:05"))

	// Silinecek kayıtlar için zaman eşiğini belirliyoruz (şu andan 90 gün öncesi)
	ninetyDaysAgo := time.Now().AddDate(0, 0, -90)

	// GORM'un Delete metodunu kullanarak eski kayıtları siliyoruz.
	result := db.Where("created_at < ?", ninetyDaysAgo).Delete(&PriceHistory{})

	if result.Error != nil {
		log.Printf("Hata: Eski kayıtlar silinirken bir sorun oluştu: %v", result.Error)
	} else if result.RowsAffected > 0 {
		fmt.Printf("   -> Başarılı: %d adet eski fiyat kaydı silindi.\n", result.RowsAffected)
	} else {
		fmt.Println("   -> Silinecek eski kayıt bulunamadı.")
	}
	fmt.Println("--- Temizleme Görevi Tamamlandı ---")
}

func main() {
	fmt.Println("Fiyat Takip Servisi Başlatıldı.")
	db := setupDatabase()

	// Yeni bir cron zamanlayıcısı oluştur
	c := cron.New()

	// runScrapingJob fonksiyonunu her saat başı çalışacak şekilde zamanla
	// "0 * * * *" -> "her saatin 0. dakikasında" demektir.
	// Test için daha sık çalıştırmak istersen: "*/1 * * * *" -> "her dakikada bir"
	c.AddFunc("0 * * * *", func() { runScrapingJob(db) })

	fmt.Println("Zamanlayıcı kuruldu. Görev her dakika başı çalışacak.")

	// Programın hemen kapanmaması için zamanlayıcıyı başlat ve bekle
	c.Start()

	// Programın sürekli çalışmasını sağla
	select {}
}
