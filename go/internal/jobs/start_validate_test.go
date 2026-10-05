package jobs

import ("encoding/json"; "errors"; "os"; "path/filepath"; "testing")

func TestStartValidationPythonParity(t *testing.T) {
	t.Setenv("PURSER_AGENT", "")
	p := filepath.Join("..", "..", "..", ".claude", "hooks", "orchestrator", "tests", "fixtures", "go_jobs_start", "start.json")
	b, err := os.ReadFile(p); if err != nil { t.Fatal(err) }
	var f struct { Cases []struct { Name string `json:"name"`; Args map[string]any `json:"args"`; Result map[string]any `json:"result"`; Request map[string]any `json:"request"` } `json:"cases"` }
	if err = json.Unmarshal(b, &f); err != nil { t.Fatal(err) }
	for _, c := range f.Cases { t.Run(c.Name, func(t *testing.T) {
		o := StartValidationOptions{Agent:"codex", FixedAgent:true, ValidateAgent:func(string) error{return nil}}
		if c.Name == "invalid_config" { o.FixedAgent=false; o.ConfiguredAgent=func()(string,error){return "",errors.New("bad configuration")} }
		r, gotErr := ValidateStart(c.Args,o); var got, want any = gotErr, c.Result
		if c.Result == nil { b,_ := json.Marshal(r); var m map[string]any; _=json.Unmarshal(b,&m); got,want=m,c.Request }
		gb, _ := json.Marshal(got); wb, _ := json.Marshal(want); if string(gb) != string(wb) { t.Fatalf("got %s want %s",gb,wb) }
	}) }
}
