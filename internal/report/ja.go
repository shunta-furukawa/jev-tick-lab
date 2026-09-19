package report

// Japanese labels for the dashboard.
//
// The page is read by one person, in Japanese, usually while something is
// running with real money in it. Every raw identifier in this file is also a
// column name in the dataset (see CLAUDE.md rule 6), so nothing here renames
// anything — this is a display layer over ids that must never change.
//
// Each entry carries a label and a one-line "why this matters". The label
// alone is not enough: "anomaly 0.35" tells a reader nothing unless the page
// also says what the number is measured against and what happens when it is
// exceeded.

type term struct {
	Label string // short, for a table cell or a chip
	Why   string // one line: what it means and why it is on screen
}

// gateJA explains why a tick did not become a trade. These are the values of
// decide.Gate.
var gateJA = map[string]term{
	"passed every gate": {"通過", "どのブレーキもかからず、取引してよい状態でした"},
	"decision_age":      {"判断が古い", "モデルの回答が遅れ、答えが届く頃には別の相場になっていました"},
	"feed":              {"市場が見えない", "データが途切れたか、板がまだ一度も同期していません"},
	"halt":              {"取引所が停止中", "bitbank 自身がこの銘柄を通常取引でないと言っています"},
	"anomaly":           {"相場が異常", "モデルが「この板はおかしい」と判断。しきい値を超えると何もしません"},
	"hold_risk":         {"保有リスク高", "持っているものが危なくなったとモデルが判断し、手仕舞いに回ります"},
	"confidence":        {"確信度が不足", "モデルの答えの確率か確信度が下限に届きませんでした"},
	"pyramid":           {"すでに建玉あり", "買い増しはしない設計です"},
	"entry_quality":     {"入る場所が悪い", "方向は合っていても、今の価格で入る価値がないという判断です"},
	"fakeout":           {"だましの疑い", "ブレイクに見えて戻される可能性が高いとモデルが判断しました"},
	"spread":            {"スプレッドが広い", "往復の手数料を板の広さが上回るので、入っても勝てません"},
	"wait":              {"モデルが待ち", "単純に「今は何もするな」という答えでした"},
	"flat":              {"建玉がない", "決済の指示が出ましたが、決済するものがありません"},
	"call failed":       {"API呼び出し失敗", "モデルに聞けませんでした。記録には残っています"},
}

// intentJA is what the model asked for, decide.Intent.
var intentJA = map[string]term{
	"none":       {"何もしない", ""},
	"open_long":  {"買いで新規", "上がると見て、新しく買う"},
	"open_short": {"売りで新規", "下がると見て売る。現物では実行できません"},
	"close":      {"決済", "持っているものを手仕舞う"},
}

// verdictJA is the risk layer's answer, risk.Verdict.
var verdictJA = map[string]term{
	"trade":     {"取引可", "どの上限にも触れていません"},
	"exit_only": {"決済のみ", "新規は止めています。持っているものは出せます"},
	"halt":      {"全停止", "注文を一切出しません"},
	"":          {"—", ""},
}

// questionJA names the nine questions. The ids are the dataset's column names.
var questionJA = map[string]term{
	"regime":        {"相場つき", "トレンドか、レンジか、方向が定まらないか"},
	"momentum":      {"勢い", "今の動きに乗る力が残っているか"},
	"volatility":    {"変動の大きさ", "値幅が広がっているか縮んでいるか"},
	"fakeout_risk":  {"だましリスク", "ブレイクが戻される確率。高いと新規を止めます"},
	"book_pressure": {"板の偏り", "買い板と売り板のどちらが厚いか"},
	"trader_action": {"トレーダーの判断", "買い・売り・待ち・利確・損切りのどれを選ぶか"},
	"entry_quality": {"エントリー品質", "0〜4。低いと新規を止めます"},
	"anomalous":     {"異常度", "0〜1。この板がおかしいと感じる度合い。高いと全部止めます"},
	"hold_risk":     {"保有リスク", "0〜4。持ち続ける危険度。高いと手仕舞いに回ります"},
}

// actionJA is the model's own answer to trader_action.
var actionJA = map[string]term{
	"open_long":   {"買い", "上がると見ている"},
	"open_short":  {"売り", "下がると見ている。現物では実行できません"},
	"wait":        {"待ち", "今は何もしない"},
	"take_profit": {"利確", "含み益を確定させる"},
	"cut_loss":    {"損切り", "損を確定させて逃げる"},
	"":            {"—", ""},
}

// styleJA names an execution path.
var styleJA = map[string]term{
	"taker": {"成行（テイカー）", "板を食って必ず約定する代わりに、往復24bpsの手数料を払います"},
	"maker": {"指値（メイカー）", "板に並んでリベートを受け取る代わりに、多くは約定しません"},
	"live":  {"実取引", "bitbank に出した本物の注文です"},
}

// modeJA names the run mode, which is the single most important thing on the
// page: whether this is money or not.
var modeJA = map[string]term{
	"observe": {"観測のみ", "板と値段を見ているだけ。モデルにも聞いていません"},
	"shadow":  {"影運転", "モデルに聞いて記録するだけ。注文は一切出しません"},
	"paper":   {"模擬売買", "約定をシミュレーションしています。お金は動きません"},
	"live":    {"実取引", "本物の注文が出ています。ここの数字はお金です"},
}

// lookup falls back to the raw id rather than to an empty cell. An unknown id
// is usually a value this file has not caught up with, and showing it is more
// useful than hiding it.
func lookup(m map[string]term, key string) term {
	if t, ok := m[key]; ok {
		return t
	}
	return term{Label: key}
}

func gateLabelJA(g string) string    { return lookup(gateJA, g).Label }
func verdictLabelJA(v string) string { return lookup(verdictJA, v).Label }
func styleLabelJA(s string) string   { return lookup(styleJA, s).Label }
