package jobs

import ("encoding/json"; "errors"; "os"; "path/filepath"; "testing")

type startCase struct { Name string `json:"name"`; Args map[string]any `json:"args"`; Result map[string]any `json:"result"`; Request map[string]any `json:"request"`; Existing bool `json:"existing"`; Calls []map[string]any `json:"calls"`; Files map[string]any `json:"files"`; Launches []map[string]any `json:"launches"` }

func TestStartValidationPythonParity(t *testing.T) {
	t.Setenv("PURSER_AGENT", "")
	p:=filepath.Join("..","..","..",".claude","hooks","orchestrator","tests","fixtures","go_jobs_start","start.json"); b,e:=os.ReadFile(p);if e!=nil{t.Fatal(e)}
	var f struct{Cases []startCase `json:"cases"`};if e=json.Unmarshal(b,&f);e!=nil{t.Fatal(e)}
	for _,c:=range f.Cases{t.Run(c.Name,func(t *testing.T){
		if c.Calls!=nil{replayStart(t,c);return}
		o:=StartValidationOptions{Agent:"codex",FixedAgent:true,ValidateAgent:func(string)error{return nil}};if c.Name=="invalid_config"{o.FixedAgent=false;o.ConfiguredAgent=func()(string,error){return "",errors.New("bad configuration")}};if c.Existing{o.LookupExisting=func(string)map[string]any{return c.Result}}
		r,got:=ValidateStart(c.Args,o);var want any=c.Result;if c.Existing{got=r.ExistingResult}else if c.Result==nil{b,_:=json.Marshal(r);_=json.Unmarshal(b,&got);want=c.Request};if !sameStart(got,want){t.Fatalf("got %#v want %#v",got,want)}
	})}
}

func replayStart(t *testing.T,c startCase){
	state:=t.TempDir();launched:=[]map[string]any{};opt:=StartOptions{Validation:StartValidationOptions{Agent:"codex",FixedAgent:true,ValidateAgent:func(string)error{return nil}},Launch:func(id string,g int64,_ string)error{launched=append(launched,map[string]any{"jobId":id,"generation":g,"token":"TOKEN"});return nil},Await:func(id string,_ *Record)map[string]any{return jobResult(map[string]any{"jobId":id,"state":"QUEUED","acceptedAt":10,"deadlineAt":20})}}
	var got map[string]any;for _,a:=range c.Calls{var e error;got,_,e=Start(state,a,opt);if e!=nil{t.Fatal(e)}}
	dir:=filepath.Join(state,"codex_exec_mcp","jobs");files:=map[string]any{};entries,_:=os.ReadDir(dir);for _,p:=range entries{if p.IsDir(){files[p.Name()]=true}else{b,_:=os.ReadFile(filepath.Join(dir,p.Name()));var v any;_=json.Unmarshal(b,&v);files[p.Name()]=normalizeStart(v)}};if len(files)==0{files=nil};if !sameStart(normalizeStart(got),c.Result)||!sameStart(files,c.Files)||!sameStart(launched,c.Launches){t.Fatalf("reservation parity mismatch: %#v",got)}
}

func normalizeStart(v any)any{switch x:=v.(type){case map[string]any:for k,y:=range x{if k=="acceptedAt"{x[k]="TIME"}else if k=="deadlineAt"{x[k]="DEADLINE"}else if k=="signalToken"||k=="ownershipToken"{x[k]="TOKEN"}else{x[k]=normalizeStart(y)}};return x;case []any:for i:=range x{x[i]=normalizeStart(x[i])};return x};return v}
func sameStart(a,b any)bool{x,_:=json.Marshal(a);y,_:=json.Marshal(b);return string(x)==string(y)}
