// Package workload generates the labelled reference corpus and replays it against a
// gateway [ENV-05, ENV-06].
package workload

import (
	"bufio"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"os"
	"strings"
)

// Item is one labelled prompt. Difficulty is the minimum tier expected to answer it
// reliably; Grader says how to score a response against Reference.
type Item struct {
	ID         string          `json:"id"`
	Category   string          `json:"category"`
	Difficulty int             `json:"difficulty"`
	System     string          `json:"system,omitempty"`
	Prompt     string          `json:"prompt"`
	Images     []string        `json:"images,omitempty"`
	Tools      json.RawMessage `json:"tools,omitempty"`
	Schema     json.RawMessage `json:"schema,omitempty"`
	Reference  string          `json:"reference"`
	Grader     string          `json:"grader"` // contains | exact | json_schema | tool_call | non_empty | regex
	Group      string          `json:"group"`  // paraphrase group
	Paraphrase bool            `json:"paraphrase,omitempty"`
	MaxTokens  int             `json:"max_tokens,omitempty"`
	Tags       []string        `json:"tags,omitempty"`
}

// Messages renders the item as OpenAI chat messages.
func (it *Item) Messages() []map[string]any {
	var msgs []map[string]any
	if it.System != "" {
		msgs = append(msgs, map[string]any{"role": "system", "content": it.System})
	}
	if len(it.Images) > 0 {
		parts := []map[string]any{{"type": "text", "text": it.Prompt}}
		for _, img := range it.Images {
			parts = append(parts, map[string]any{"type": "image_url", "image_url": map[string]any{"url": img}})
		}
		msgs = append(msgs, map[string]any{"role": "user", "content": parts})
	} else {
		msgs = append(msgs, map[string]any{"role": "user", "content": it.Prompt})
	}
	return msgs
}

// Body renders the item as a chat completions request body for model.
func (it *Item) Body(model string) map[string]any {
	b := map[string]any{"model": model, "messages": it.Messages(), "temperature": 0}
	if len(it.Tools) > 0 {
		b["tools"] = json.RawMessage(it.Tools)
	}
	if len(it.Schema) > 0 {
		b["response_format"] = map[string]any{"type": "json_schema", "json_schema": map[string]any{"name": "answer", "schema": json.RawMessage(it.Schema)}}
	}
	if it.MaxTokens > 0 {
		b["max_tokens"] = it.MaxTokens
	}
	return b
}

const pixel = "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNkYPhfDwAChwGA60e6kgAAAABJRU5ErkJggg=="

type gen struct {
	r     *rand.Rand
	items []Item
	n     map[string]int
}

func (g *gen) pick(xs []string) string { return xs[g.r.IntN(len(xs))] }

func (g *gen) add(it Item) {
	g.n[it.Category]++
	if it.ID == "" {
		it.ID = fmt.Sprintf("%s-%03d", it.Category, g.n[it.Category])
	}
	if it.Group == "" {
		it.Group = it.ID
	}
	g.items = append(g.items, it)
}

// Generate builds the reference corpus deterministically from seed.
func Generate(seed uint64) []Item {
	g := &gen{r: rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15)), n: map[string]int{}}
	g.faq()
	g.greetings()
	g.classification()
	g.extraction()
	g.summarisation()
	g.jsonSchema()
	g.toolUse()
	g.code()
	g.reasoning()
	g.images()
	g.longContext()
	g.paraphrases(90)
	return g.items
}

type business struct {
	name, kind, hours, delivery, returns, location, payment, phone, parking, wifi string
}

var businesses = []business{
	{"Lotus Bakery", "bakery", "7am to 9pm every day", "free delivery on orders above 500 rupees", "fresh items cannot be returned", "Koregaon Park", "UPI, cash and all major cards", "020-4455-1001", "street parking only", "free Wi-Fi for customers"},
	{"Banyan Clinic", "clinic", "8am to 8pm Monday to Saturday", "home sample collection within 10 km", "consultation fees are not refundable", "Baner Road", "UPI and cards, no cheques", "020-4455-2002", "basement parking for patients", "no public Wi-Fi"},
	{"IronPeak Gym", "gym", "5am to 11pm on weekdays and 6am to 8pm on weekends", "no delivery service", "memberships can be paused but not refunded", "Viman Nagar", "cards and monthly auto-debit", "020-4455-3003", "free parking for members", "free Wi-Fi in the lounge"},
	{"Granth Library", "library", "10am to 7pm, closed on Mondays", "book delivery for members at 40 rupees", "late returns cost 5 rupees per day", "Deccan Gymkhana", "cash and UPI", "020-4455-4004", "two-wheeler parking only", "free Wi-Fi with a library card"},
	{"Monsoon Electronics", "store", "11am to 9pm, closed on Tuesdays", "free delivery within the city", "30-day returns with the original receipt", "MG Road", "Visa, Mastercard, RuPay and UPI", "020-4455-5005", "paid parking in the mall", "no public Wi-Fi"},
}

func (b business) system() string {
	return fmt.Sprintf("You are the customer assistant for %s, a %s. Facts: Opening hours: %s. Delivery: %s. Returns and refunds: %s. Location: %s. Payment: %s. Phone: %s. Parking: %s. Wi-Fi: %s. Answer briefly using only these facts.",
		b.name, b.kind, b.hours, b.delivery, b.returns, b.location, b.payment, b.phone, b.parking, b.wifi)
}

func (g *gen) faq() {
	type qa struct {
		q    string
		pick func(b business) string
	}
	qs := []qa{
		{"What are the opening hours of %s?", func(b business) string { return b.hours }},
		{"When does %s open?", func(b business) string { return b.hours }},
		{"Does %s deliver?", func(b business) string { return b.delivery }},
		{"What is the return policy at %s?", func(b business) string { return b.returns }},
		{"Where is %s located?", func(b business) string { return b.location }},
		{"Which payment methods does %s accept?", func(b business) string { return b.payment }},
		{"What is the phone number of %s?", func(b business) string { return b.phone }},
		{"Is there parking at %s?", func(b business) string { return b.parking }},
		{"Does %s have Wi-Fi?", func(b business) string { return b.wifi }},
		{"Can I pay by card at %s?", func(b business) string { return b.payment }},
		{"How do I reach %s by phone?", func(b business) string { return b.phone }},
		{"Is %s open in the evening?", func(b business) string { return b.hours }},
		{"Can I get a refund from %s?", func(b business) string { return b.returns }},
		{"Which area is %s in?", func(b business) string { return b.location }},
		{"Do you have home delivery at %s?", func(b business) string { return b.delivery }},
		{"Where can I park when visiting %s?", func(b business) string { return b.parking }},
		{"Is Wi-Fi available at %s?", func(b business) string { return b.wifi }},
		{"What time does %s close?", func(b business) string { return b.hours }},
		{"Do they take UPI at %s?", func(b business) string { return b.payment }},
		{"What's the address area for %s?", func(b business) string { return b.location }},
	}
	for _, b := range businesses {
		for _, q := range qs {
			g.add(Item{Category: "faq", Difficulty: 0, System: b.system(), Prompt: fmt.Sprintf(q.q, b.name),
				Reference: q.pick(b), Grader: "contains", MaxTokens: 80, Tags: []string{"faq"}})
		}
	}
}

func (g *gen) greetings() {
	for _, p := range []string{"Hi!", "Hello there", "Good morning!", "Hey, how are you?", "Thanks a lot!", "Thank you so much",
		"Good evening", "Hi team", "Hello!", "Cheers!", "Ok, thanks", "Great, thank you!", "Hey there!", "Good afternoon",
		"Bye for now", "Thanks, that helps", "Hello, anyone there?", "Hi, quick hello", "Thank you!", "Hey!",
		"Good morning team", "Thanks again", "Hello friend", "Hi :)", "Okay cool", "Thanks, bye", "Hey, good morning",
		"Hello and welcome", "Hi again", "Great, cheers"} {
		g.add(Item{Category: "greeting", Difficulty: 0, Prompt: p, Reference: "Hello! How can I help you today?", Grader: "non_empty", MaxTokens: 40})
	}
}

var products = []string{"blender", "headphones", "phone case", "office chair", "rice cooker", "laptop bag", "water bottle", "desk lamp", "running shoes", "backpack"}

func (g *gen) classification() {
	pos := []string{"works perfectly and arrived early", "is excellent value and I love it", "exceeded my expectations", "is sturdy, quiet and beautifully made", "was a delight to use from day one"}
	neg := []string{"broke after two days", "is noisy and the seller ignored my complaint", "arrived damaged and smelled of smoke", "stopped charging within a week", "is flimsy and a waste of money"}
	neu := []string{"arrived on Tuesday in a brown box", "is the same model as my previous one", "comes in three colours", "was delivered by courier", "has a one-year warranty card inside"}
	for i := 0; i < 50; i++ {
		label, phrase := "positive", g.pick(pos)
		switch i % 3 {
		case 1:
			label, phrase = "negative", g.pick(neg)
		case 2:
			label, phrase = "neutral", g.pick(neu)
		}
		p := fmt.Sprintf("Classify the sentiment of this product review as positive, negative or neutral. Reply with one word.\nReview: \"The %s %s.\" (order %d)", products[i%len(products)], phrase, 1000+i)
		g.add(Item{Category: "classification", Difficulty: 1, Prompt: p, Reference: label, Grader: "contains", MaxTokens: 5})
	}
}

var companies = []string{"Sahyadri Traders", "Kaveri Foods", "Nimbus Labs", "Orchid Textiles", "Pune Metro Supplies", "Vistara Paints", "Indus Logistics", "Coral Pharma"}
var months = []string{"January", "February", "March", "April", "May", "June", "July", "August", "September", "October", "November", "December"}

func (g *gen) extraction() {
	for i := 0; i < 50; i++ {
		inv := fmt.Sprintf("INV-%04d", 2000+g.r.IntN(7999))
		amt := fmt.Sprintf("%d,%03d", 1+g.r.IntN(98), g.r.IntN(1000))
		comp := g.pick(companies)
		date := fmt.Sprintf("%d %s", 1+g.r.IntN(28), g.pick(months))
		var text string
		switch i % 3 {
		case 0:
			text = fmt.Sprintf("Invoice %s was issued to %s on %s; the total due is ₹%s.", inv, comp, date, amt)
		case 1:
			text = fmt.Sprintf("Dear %s, please find attached %s dated %s for ₹%s payable within 15 days.", comp, inv, date, amt)
		default:
			text = fmt.Sprintf("Payment reminder: %s (%s) — amount outstanding ₹%s, raised on %s.", inv, comp, amt, date)
		}
		p := "Extract the invoice number and the total amount from this text. Answer as: <invoice number>; <amount>\n\n" + text
		g.add(Item{Category: "extraction", Difficulty: 1, Prompt: p, Reference: inv + "; ₹" + amt, Grader: "contains", MaxTokens: 30})
	}
}

var topics = []struct{ key, lead string }{
	{"solar panels", "Rooftop solar panels have cut electricity bills for many housing societies in Pune."},
	{"rainwater harvesting", "Rainwater harvesting systems are now mandatory for new buildings in several cities."},
	{"electric buses", "The city transport authority is replacing diesel buses with electric buses."},
	{"millets", "Millets are returning to Indian kitchens as a climate-resilient grain."},
	{"cybersecurity training", "Companies are investing in cybersecurity training after a rise in phishing attacks."},
	{"metro rail", "The new metro rail line has reduced commute times across the city."},
	{"urban gardening", "Urban gardening on balconies and terraces is growing among young families."},
	{"remote work", "Remote work has changed how small IT firms hire and manage teams."},
	{"air quality", "Winter air quality has worsened because of construction dust and vehicle emissions."},
	{"digital payments", "Digital payments now account for most small retail transactions."},
}

var filler = []string{
	"Residents say the change took some getting used to.", "Officials expect the trend to continue over the next five years.",
	"Experts point to falling costs as the main driver.", "Some critics argue the benefits are unevenly distributed.",
	"Pilot projects in two neighbourhoods were considered a success.", "Local suppliers have expanded their teams to meet demand.",
	"Surveys show strong support among people under forty.", "Funding came partly from state schemes and partly from private investors.",
	"Maintenance remains a concern for older buildings.", "Several schools have added the topic to their curriculum.",
	"Banks have introduced special loans to encourage adoption.", "The data will be reviewed again at the end of the year.",
	"Smaller towns are watching the results closely.", "Industry groups have published new guidelines.",
}

func (g *gen) summarisation() {
	for i := 0; i < 40; i++ {
		t := topics[i%len(topics)]
		n := 6 + g.r.IntN(3)
		diff := 1
		if i%2 == 1 {
			n, diff = 60+g.r.IntN(20), 2
		}
		sents := []string{t.lead}
		for j := 0; j < n; j++ {
			sents = append(sents, g.pick(filler))
		}
		p := fmt.Sprintf("Summarize the following passage in one sentence (report %d):\n\n%s", 100+i, strings.Join(sents, " "))
		g.add(Item{Category: "summarisation", Difficulty: diff, Prompt: p, Reference: t.key, Grader: "contains", MaxTokens: 80})
	}
}

var names = []string{"Asha", "Rohan", "Meera", "Kabir", "Ishaan", "Priya", "Arjun", "Neha", "Vikram", "Zoya"}
var cities = []string{"Pune", "Nagpur", "Kochi", "Indore", "Mysuru", "Jaipur", "Surat", "Bhopal", "Goa", "Shillong"}

const personSchema = `{"type":"object","properties":{"name":{"type":"string"},"age":{"type":"integer"},"city":{"type":"string"}},"required":["name","age","city"],"additionalProperties":false}`

func (g *gen) jsonSchema() {
	for i := 0; i < 30; i++ {
		n, c, age := names[i%len(names)], cities[(i*3)%len(cities)], 21+g.r.IntN(50)
		p := fmt.Sprintf("Extract the person's name, age and city as JSON: \"%s, aged %d, moved to %s last year for work (ref %d).\"", n, age, c, 500+i)
		g.add(Item{Category: "json_schema", Difficulty: 1, Prompt: p, Schema: json.RawMessage(personSchema), Reference: "", Grader: "json_schema", MaxTokens: 60})
	}
}

const weatherTool = `[{"type":"function","function":{"name":"get_weather","description":"Current weather for a city","parameters":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}}}]`
const bookTool = `[{"type":"function","function":{"name":"book_table","description":"Book a restaurant table","parameters":{"type":"object","properties":{"restaurant":{"type":"string"},"people":{"type":"integer"},"time":{"type":"string"}},"required":["restaurant","people","time"]}}}]`

func (g *gen) toolUse() {
	for i := 0; i < 30; i++ {
		if i%2 == 0 {
			c := cities[i%len(cities)]
			g.add(Item{Category: "tool_use", Difficulty: 1, Prompt: fmt.Sprintf("What's the weather like in %s right now? (trip %d)", c, i), Tools: json.RawMessage(weatherTool), Reference: "get_weather", Grader: "tool_call"})
		} else {
			g.add(Item{Category: "tool_use", Difficulty: 1, Prompt: fmt.Sprintf("Book a table for %d people at Saffron House at %d:30 pm tonight.", 2+i%5, 6+i%4), Tools: json.RawMessage(bookTool), Reference: "book_table", Grader: "tool_call"})
		}
	}
}

func (g *gen) code() {
	specs := []struct{ fn, spec string }{
		{"is_palindrome", "returns True if a string reads the same backwards, ignoring case and spaces"},
		{"chunk_list", "splits a list into chunks of size n"},
		{"word_count", "counts how often each word appears in a text and returns a dict"},
		{"flatten", "flattens an arbitrarily nested list of integers"},
		{"merge_intervals", "merges overlapping intervals given as (start, end) tuples"},
		{"parse_duration", "parses strings like '1h30m' into a number of seconds"},
		{"top_k", "returns the k most frequent elements of a list"},
		{"validate_pan", "checks whether a string is a well-formed Indian PAN number"},
		{"rolling_mean", "computes a rolling mean over a window of size w"},
		{"dedupe_emails", "removes duplicate email addresses case-insensitively while keeping order"},
	}
	for i := 0; i < 30; i++ {
		s := specs[i%len(specs)]
		lang := []string{"Python", "Python", "Go"}[i%3]
		fn := s.fn
		if lang == "Go" {
			fn = goName(s.fn)
		}
		p := fmt.Sprintf("Write a %s function `%s` that %s. Include a short docstring. (task %d)", lang, fn, s.spec, 300+i)
		ref := "def " + fn
		if lang == "Go" {
			ref = "func " + fn
		}
		g.add(Item{Category: "code", Difficulty: 2, Prompt: p, Reference: ref, Grader: "contains", MaxTokens: 400})
	}
	bugs := []struct{ code, bug string }{
		{"func sum(xs []int) int {\n\ttotal := 0\n\tfor i := 0; i <= len(xs); i++ {\n\t\ttotal += xs[i]\n\t}\n\treturn total\n}", "off-by-one"},
		{"def average(xs):\n    return sum(xs) / len(xs) if xs else sum(xs) / 0", "division by zero"},
		{"func get(m map[string]*User, k string) string {\n\treturn m[k].Name\n}", "nil pointer"},
		{"def append_item(item, items=[]):\n    items.append(item)\n    return items", "mutable default"},
		{"for i := 0; i < 3; i++ {\n\tgo func() { fmt.Println(i) }()\n}", "race"},
	}
	for i := 0; i < 20; i++ {
		b := bugs[i%len(bugs)]
		p := fmt.Sprintf("The following code has a bug (ticket %d):\n```\n%s\n```\nFind the bug, explain step by step why it happens, then write a corrected version and unit tests for it.", 900+i, b.code)
		g.add(Item{Category: "code", Difficulty: 3, Prompt: p, Reference: b.bug, Grader: "contains", MaxTokens: 600})
	}
}

func goName(s string) string {
	parts := strings.Split(s, "_")
	for i, p := range parts {
		parts[i] = strings.ToUpper(p[:1]) + p[1:]
	}
	return strings.Join(parts, "")
}

func (g *gen) reasoning() {
	for i := 0; i < 30; i++ {
		price, n := 12+g.r.IntN(30), 3+g.r.IntN(8)
		bill := ((price*n)/100 + 1) * 100
		change := bill - price*n
		p := fmt.Sprintf("A shop sells notebooks at ₹%d each. If I buy %d notebooks and pay with a ₹%d note, how much change do I get?", price, n, bill)
		g.add(Item{Category: "reasoning", Difficulty: 2, Prompt: p, Reference: fmt.Sprint(change), Grader: "contains", MaxTokens: 120})
	}
	for i := 0; i < 20; i++ {
		dep := 6 + g.r.IntN(4)
		mins := []int{10, 20, 30, 40}[g.r.IntN(4)]
		dist, speed, wait := 120+30*g.r.IntN(5), 60, 15+5*g.r.IntN(3)
		travel := dist * 60 / speed
		total := mins + travel + wait
		h, m := dep+total/60, total%60
		p := fmt.Sprintf("Reason step by step. A train leaves at %d:%02d, travels %d km at %d km/h, then stops for %d minutes before arriving. "+
			"First compute each leg, then explain why your total is correct. At what time does it arrive (HH:MM, 24-hour)? (case %d)", dep, mins, dist, speed, wait, 700+i)
		g.add(Item{Category: "reasoning", Difficulty: 3, Prompt: p, Reference: fmt.Sprintf("%d:%02d", h, m), Grader: "contains", MaxTokens: 400})
	}
}

func (g *gen) images() {
	descs := []struct{ ask, ref string }{
		{"What colour is the single pixel in this image?", "red"},
		{"Describe this chart in one sentence.", "bar chart"},
		{"Read the text in this screenshot.", "invoice"},
		{"How many people are in this photo?", "two"},
		{"What product is shown in this picture?", "headphones"},
	}
	for i := 0; i < 15; i++ {
		d := descs[i%len(descs)]
		g.add(Item{Category: "image", Difficulty: 2, Prompt: fmt.Sprintf("%s (image %d)", d.ask, i), Images: []string{pixel}, Reference: d.ref, Grader: "contains", MaxTokens: 60})
	}
}

func (g *gen) longContext() {
	for i := 0; i < 10; i++ {
		word := []string{"MARIGOLD", "KESTREL", "OBSIDIAN", "TAMARIND", "MONSOON", "SAFFRON", "CITADEL", "LANTERN", "QUARTZ", "VERMILION"}[i]
		// Finding one fact in ~6k tokens is tier-1 work; at ~17k tokens it no longer
		// fits small context windows.
		sections, diff := 160, 1
		if i%2 == 1 {
			sections, diff = 420, 2
		}
		var sb strings.Builder
		for s := 1; s <= sections; s++ {
			fmt.Fprintf(&sb, "Section %d. ", s)
			if s == sections/2 {
				fmt.Fprintf(&sb, "The project code word is %s. ", word)
			}
			for k := 0; k < 3; k++ {
				sb.WriteString(filler[(s*7+k*3+i)%len(filler)] + " ")
			}
			sb.WriteString("\n")
		}
		p := sb.String() + fmt.Sprintf("\nQuestion (doc %d): what is the project code word mentioned in the document?", i)
		g.add(Item{Category: "long_context", Difficulty: diff, Prompt: p, Reference: word, Grader: "contains", MaxTokens: 20})
	}
}

// paraphrases adds reworded copies of earlier items sharing their group and answer.
func (g *gen) paraphrases(n int) {
	base := append([]Item(nil), g.items...)
	wrappers := []func(string) string{
		func(s string) string { return "Could you please help with this: " + s },
		func(s string) string { return "Quick question — " + s },
		func(s string) string { return s + " Thanks!" },
		func(s string) string { return "Hey, " + strings.ToLower(s[:1]) + s[1:] },
		func(s string) string { return "I was wondering: " + s },
	}
	step := len(base) / n
	for k := 0; k < n; k++ {
		src := base[k*step]
		if src.Category == "long_context" {
			continue
		}
		it := src
		it.ID = ""
		it.Paraphrase = true
		it.Group = src.ID
		it.Prompt = wrappers[k%len(wrappers)](src.Prompt)
		g.add(it)
	}
}

// Save writes items as JSONL.
func Save(path string, items []Item) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	w := bufio.NewWriter(f)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	for _, it := range items {
		if err := enc.Encode(it); err != nil {
			return err
		}
	}
	return w.Flush()
}

// Load reads a JSONL corpus.
func Load(path string) ([]Item, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var items []Item
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 4<<20), 64<<20)
	for sc.Scan() {
		if len(strings.TrimSpace(sc.Text())) == 0 {
			continue
		}
		var it Item
		if err := json.Unmarshal(sc.Bytes(), &it); err != nil {
			return nil, err
		}
		items = append(items, it)
	}
	return items, sc.Err()
}
