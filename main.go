package main

import (
	"encoding/json"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"net/smtp"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/bregydoc/gtranslate"
)

// --- CONFIGURACIÓN GLOBAL ---
const (
	Port           = ":8080"
	DefaultLang    = "es"
	DefaultCountry = "US"
	PageSize       = 20
)

var (
	productosOriginales []Producto
	cacheTraducida      = make(map[string]map[string][]Producto)
	tasasCambio         = map[string]float64{"USD": 1.0, "MXN": 17.50, "COP": 3950.0, "GTQ": 7.80, "PEN": 3.75}
	monedas             = map[string]string{"MX": "MXN", "GT": "GTQ", "PE": "PEN", "CO": "COP", "US": "USD"}
	simbolos            = map[string]string{"MX": "$", "GT": "Q", "PE": "S/", "CO": "$", "US": "$"}
)

// --- MODELOS ---

type Producto struct {
	URL             string            `json:"url"`
	Categoria       string            `json:"categoria"`
	Titulo          string            `json:"titulo"`
	ImagenesGrandes []string          `json:"imagenes"`
	Colores         []Color           `json:"colores"`
	Tamaños         []string          `json:"tamaños"`
	Precios         []Precio          `json:"precios"`
	BloqueLogistico string            `json:"bloque_logistico"`
	Detalles        map[string]string `json:"detalles,omitempty"`
	MainImage       string
	MainPrice       string
	LogisticaInfo   string
	ID              string
}

type Color struct {
	Nombre string `json:"nombre"`
	Imagen string `json:"imagen,omitempty"`
}

type Precio struct {
	Cantidad string `json:"cantidad"`
	Valor    string `json:"valor"`
}

type PageData struct {
	Categorias   []string
	Productos    []Producto
	Producto     *Producto
	CurrentCat   string
	CurrentPage  int
	TotalPages   int
	PaisActual   string
	Idioma       string
	SearchQuery  string
	Tasas        map[string]float64
	Tasa         float64
	Traducciones map[string]string
}

type CheckoutPayload struct {
	IDPaypal    string  `json:"id_transaccion_paypal"`
	Nombre      string  `json:"nombre"`
	Email       string  `json:"email"`
	Telefono    string  `json:"telefono"`
	Direccion   string  `json:"direccion"`
	Pais        string  `json:"pais"`
	TotalPagado float64 `json:"total_pagado"`
	Items       []struct {
		Titulo   string `json:"titulo"`
		Cantidad int    `json:"cantidad"`
		URL      string `json:"url"` // <--- Agregado para rastrear el producto
	} `json:"items"`
}

var traducciones = map[string]map[string]string{
	"es": {
		"finalizar_compra": "Finalizar Compra",
		"info_envio":       "Información de Envío",
		"pais_region":      "País o Región",
		"nombre":           "Nombre",
		"apellido":         "Apellido",
		"email":            "Email de Contacto",
		"telefono":         "Número de Teléfono",
		"direccion":        "Dirección Completa",
		"ciudad":           "Ciudad",
		"estado":           "Estado",
		"cp":               "C.P.",
		"tu_pedido":        "Tu Pedido",
		"envio_total":      "Envío",
		"exito_titulo":     "¡Gracias por su compra!",
		"exito_msg":        "Hemos recibido su pedido correctamente. Se ha enviado un comprobante a su correo electrónico.",
		"id_pedido":        "ID del Pedido",
		"fecha_compra":     "Fecha de Compra",
		"entrega_estimada": "Fecha Estimada de Entrega",
		"aproximadamente":  "Aproximadamente el",
		"nota_entrega":     "El plazo de entrega es de 15 días laborables aproximadamente.",
		"seguir_comprando": "Seguir Comprando",
		"imprimir":         "Imprimir Recibo",
	},
	"en": {
		"finalizar_compra": "Checkout",
		"info_envio":       "Shipping Information",
		"pais_region":      "Country or Region",
		"nombre":           "First Name",
		"apellido":         "Last Name",
		"email":            "Contact Email",
		"telefono":         "Phone Number",
		"direccion":        "Full Address",
		"ciudad":           "City",
		"estado":           "State/Province",
		"cp":               "Zip/Postal Code",
		"tu_pedido":        "Your Order",
		"envio_total":      "Shipping",
		"exito_titulo":     "Thank you for your purchase!",
		"exito_msg":        "Your order has been received. A confirmation has been sent to your email.",
		"id_pedido":        "Order ID",
		"fecha_compra":     "Purchase Date",
		"entrega_estimada": "Estimated Delivery Date",
		"aproximadamente":  "Approximately on",
		"nota_entrega":     "Delivery time is approximately 15 business days.",
		"seguir_comprando": "Continue Shopping",
		"imprimir":         "Print Receipt",
	},
}

// --- LOGICA DE NEGOCIO ---

func slugify(s string) string {
	return strings.ToLower(regexp.MustCompile(`[^a-z0-9]+`).ReplaceAllString(s, "-"))
}

func getLocalCurrency(pais string) (string, string, float64) {
	mon := monedas[pais]
	if mon == "" {
		mon = "USD"
	}
	simb := simbolos[pais]
	if simb == "" {
		simb = "$"
	}
	return mon, simb, tasasCambio[mon]
}

func formatCurrency(val float64, mon string, simb string) string {
	if mon == "COP" {
		return fmt.Sprintf("%s%s %s", simb, formatearMiles(val), mon)
	}
	return fmt.Sprintf("%s%.2f %s", simb, val, mon)
}

func formatearMiles(n float64) string {
	s := fmt.Sprintf("%.0f", n)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "." + s[i:]
	}
	return s
}

func procesarVista(p Producto, lang, pais string) Producto {
	mon, simb, tasa := getLocalCurrency(pais)

	// 1. Extraer precio unitario base
	rawVal := regexp.MustCompile(`[0-9.]+`).FindString(p.Precios[0].Valor)
	valUnitarioUSD, _ := strconv.ParseFloat(rawVal, 64)
	if valUnitarioUSD == 0 {
		valUnitarioUSD = 10.0
	}

	// 2. Lógica MOQ y Fórmula (Cantidad * Valor + 4.50 USD)
	cantidadMOQ := 1.0
	reMOQ := regexp.MustCompile(`(?i)Minimum\s+order\s+quantity:\s*(\d+)`)
	match := reMOQ.FindStringSubmatch(p.Precios[0].Cantidad)
	if len(match) > 1 {
		if val, err := strconv.ParseFloat(match[1], 64); err == nil {
			cantidadMOQ = val
		}
	}

	// Aplicamos la fórmula: (MOQ * PrecioUnitario) + 4.50 USD fijos
	totalUSD := (cantidadMOQ * valUnitarioUSD) + 4.50

	// 3. Convertir a moneda local y asignar
	p.MainPrice = formatCurrency(totalUSD*tasa, mon, simb)
	p.ID = slugify(p.Titulo)
	p.LogisticaInfo = traducirLogistica(p.BloqueLogistico, lang, tasa, mon, simb)

	if len(p.ImagenesGrandes) > 0 {
		p.MainImage = p.ImagenesGrandes[0]
	}
	return p
}

func traducirLogistica(bloque, lang string, tasa float64, mon, simb string) string {
	if bloque == "" {
		return ""
	}

	bloqueLower := strings.ToLower(bloque)
	valorPivoteUSD := 17.30
	const (
		factorStandard = 0.83
		factorEconomy  = 0.60
	)

	var costoFinalUSD float64
	var tipoEnvio string
	var diasEntrega int

	if strings.Contains(bloqueLower, "premium") {
		costoFinalUSD = valorPivoteUSD
		tipoEnvio = "Premium"
		diasEntrega = 7
	} else if strings.Contains(bloqueLower, "standard") || strings.Contains(bloqueLower, "estándar") {
		costoFinalUSD = valorPivoteUSD * factorStandard
		tipoEnvio = "Estándar"
		diasEntrega = 15
	} else {
		costoFinalUSD = valorPivoteUSD * factorEconomy
		tipoEnvio = "Económico"
		diasEntrega = 30
	}

	textoPrecio := formatCurrency(costoFinalUSD*tasa, mon, simb)
	fechaEstimada := time.Now().AddDate(0, 0, diasEntrega)

	meses := map[string]string{
		"Jan": "Ene", "Feb": "Feb", "Mar": "Mar", "Apr": "Abr", "May": "May", "Jun": "Jun",
		"Jul": "Jul", "Aug": "Ago", "Sep": "Sep", "Oct": "Oct", "Nov": "Nov", "Dec": "Dic",
	}

	fechaFmt := fechaEstimada.Format("02 Jan")
	if lang == "es" {
		for en, es := range meses {
			fechaFmt = strings.Replace(fechaFmt, en, es, 1)
		}
	}

	if lang == "es" {
		return fmt.Sprintf("Envío %s: %s (Llega el %s)", tipoEnvio, textoPrecio, fechaFmt)
	}
	tipoEng := map[string]string{"Premium": "Premium", "Estándar": "Standard", "Económico": "Economy"}[tipoEnvio]
	return fmt.Sprintf("%s Shipping: %s (Arrives %s)", tipoEng, textoPrecio, fechaFmt)
}

// --- CARGA DE DATOS ---

func cargarDatos() {
	log.Println("Iniciando carga de catálogo...")
	file, err := os.ReadFile("./data/productos.json")
	if err != nil {
		log.Fatal("No se encontró productos.json:", err)
	}
	json.Unmarshal(file, &productosOriginales)

	paises := []string{"US", "MX", "CO", "GT", "PE"}
	idiomas := []string{"en", "es"}

	for _, lang := range idiomas {
		cacheTraducida[lang] = make(map[string][]Producto)
		for _, pais := range paises {
			var lista []Producto
			for _, p := range productosOriginales {
				pMod := p
				if lang == "es" {
					t, _ := gtranslate.TranslateWithParams(p.Titulo, gtranslate.TranslationParams{From: "en", To: "es"})
					if t != "" {
						pMod.Titulo = t
					}
				}
				lista = append(lista, procesarVista(pMod, lang, pais))
			}
			cacheTraducida[lang][pais] = lista
		}
	}
	log.Println("Catálogo listo.")
}

func enviarEmailConfirmacion(to string, orderID string) {
	// ESTO ES UN EJEMPLO - Requiere credenciales reales
	from := "tu-email@gmail.com"
	pass := "tu-contraseña-de-aplicacion"

	msg := "Subject: Confirmación de Pedido " + orderID + "\n" +
		"Muchas gracias por su compra.\n" +
		"Su pedido llegará en aproximadamente 15 días."

	err := smtp.SendMail("smtp.gmail.com:587",
		smtp.PlainAuth("", from, pass, "smtp.gmail.com"),
		from, []string{to}, []byte(msg))

	if err != nil {
		log.Printf("Error enviando email: %s", err)
	}
}

// --- HANDLERS ---

func renderTemplate(w http.ResponseWriter, tmplName string, data PageData) {
	funcMap := template.FuncMap{
		"add": func(a, b int) int { return a + b },
		"sub": func(a, b int) int { return a - b },
	}
	tmpl, err := template.New("layout").Funcs(funcMap).ParseFiles(
		"./static/layout.html",
		"./static/"+tmplName,
	)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	tmpl.ExecuteTemplate(w, "layout", data)
}

func homeHandler(w http.ResponseWriter, r *http.Request) {
	lang, country := getParams(r)
	cat := r.URL.Query().Get("cat")
	q := strings.ToLower(r.URL.Query().Get("q"))
	todosLosProductos := cacheTraducida[lang][country]
	mapCat := make(map[string]bool)
	var listaCategorias []string
	for _, p := range todosLosProductos {
		if p.Categoria != "" && !mapCat[p.Categoria] {
			mapCat[p.Categoria] = true
			listaCategorias = append(listaCategorias, p.Categoria)
		}
	}
	var filtrados []Producto
	for _, p := range todosLosProductos {
		matchCat := (cat == "" || p.Categoria == cat)
		matchSearch := (q == "" || strings.Contains(strings.ToLower(p.Titulo), q))
		if matchCat && matchSearch {
			filtrados = append(filtrados, p)
		}
	}
	page, _ := strconv.Atoi(r.URL.Query().Get("p"))
	if page < 1 {
		page = 1
	}
	start := (page - 1) * PageSize
	if start > len(filtrados) {
		start = 0
	}
	end := start + PageSize
	if end > len(filtrados) {
		end = len(filtrados)
	}

	renderTemplate(w, "index.html", PageData{
		Categorias:  listaCategorias,
		Productos:   filtrados[start:end],
		CurrentCat:  cat,
		CurrentPage: page,
		TotalPages:  (len(filtrados) + PageSize - 1) / PageSize,
		PaisActual:  country,
		Idioma:      lang,
		SearchQuery: q,
	})
}

func productHandler(w http.ResponseWriter, r *http.Request) {
	lang, country := getParams(r)
	id := r.URL.Query().Get("id")
	for _, p := range cacheTraducida[lang][country] {
		if p.ID == id {
			renderTemplate(w, "details.html", PageData{Producto: &p, PaisActual: country, Idioma: lang})
			return
		}
	}
	http.NotFound(w, r)
}

func shoppingHandler(w http.ResponseWriter, r *http.Request) {
	lang, country := getParams(r)
	_, _, tasa := getLocalCurrency(country)
	renderTemplate(w, "shopping.html", PageData{
		PaisActual:   country,
		Idioma:       lang,
		Tasa:         tasa,
		Traducciones: traducciones[lang], // <--- Esto envía las palabras correctas
	})
}

func checkoutHandler(w http.ResponseWriter, r *http.Request) {
	lang, country := getParams(r)
	_, _, tasa := getLocalCurrency(country)
	renderTemplate(w, "checkout.html", PageData{
		PaisActual: country,
		Idioma:     lang,
		Tasas:      tasasCambio,
		Tasa:       tasa,
	})
}

func apiCheckoutHandler(w http.ResponseWriter, r *http.Request) {
	var p CheckoutPayload
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	orderID := fmt.Sprintf("ORD-%d", time.Now().Unix())
	mon, _, tasa := getLocalCurrency(p.Pais)
	pedidoCompleto := map[string]interface{}{
		"id_pedido":             orderID,
		"id_transaccion_paypal": p.IDPaypal,
		"fecha":                 time.Now().Format("2006-01-02 15:04:05"),
		"pais":                  p.Pais,
		"moneda":                mon,
		"cliente":               p.Nombre,
		"email":                 p.Email,
		"resumen": map[string]interface{}{
			"total_final_usd": p.TotalPagado,
			"tasa_aplicada":   tasa,
		},
		"productos": p.Items,
	}
	f, _ := os.OpenFile("pedidos.jsonl", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	defer f.Close()
	logData, _ := json.Marshal(pedidoCompleto)
	f.Write(append(logData, '\n'))
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"success": true, "id_pedido": orderID})
	enviarEmailConfirmacion(p.Email, orderID)
}

func getParams(r *http.Request) (string, string) {
	lang := r.URL.Query().Get("lang")
	country := r.URL.Query().Get("country")
	if lang == "" {
		lang = DefaultLang
	}
	if country == "" {
		country = DefaultCountry
	}
	return lang, country
}

func successHandler(w http.ResponseWriter, r *http.Request) {
	lang, country := getParams(r)
	orderID := r.URL.Query().Get("id")

	// Usamos el campo CurrentCat del struct PageData para enviar el ID del pedido
	// y no tener que modificar el struct original.
	renderTemplate(w, "success.html", PageData{
		PaisActual: country,
		Idioma:     lang,
		CurrentCat: orderID,
	})
}

func main() {
	cargarDatos()
	mux := http.NewServeMux()
	fs := http.FileServer(http.Dir("./static"))
	mux.Handle("/static/", http.StripPrefix("/static/", fs))
	mux.HandleFunc("/", homeHandler)
	mux.HandleFunc("/producto", productHandler)
	mux.HandleFunc("/shopping", shoppingHandler)
	mux.HandleFunc("/checkout", checkoutHandler)
	mux.HandleFunc("/api/process-checkout", apiCheckoutHandler)
	mux.HandleFunc("/success", successHandler)
	log.Printf("🚀 Tienda iniciada en http://localhost%s", Port)
	log.Fatal(http.ListenAndServe(Port, mux))
}
