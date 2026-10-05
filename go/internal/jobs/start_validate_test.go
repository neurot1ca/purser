package jobs

import ("encoding/json"; "errors"; "os"; "os/exec"; "path/filepath"; "strings"; "testing")

type startCase struct { Name string `json:"name"`; Args map[string]any `json:"args"`; Result map[string]any `json:"result"`; Request map[string]any `json:"request"`; Existing bool `json:"existing"`; Calls []map[string]any `json:"calls"`; Seed []map[string]any `json:"seed"`; Thread map[string]any `json:"thread"`; GitCase bool `json:"gitCase"`; Files map[string]any `json:"files"`; Launches []map[string]any `json:"launches"`; Decisions []map[string]any `json:"decisions"` }

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
	state:=t.TempDir(); expand:=map[string]string{"<ROOT>":state}; normalize:=map[string]string{state:"<ROOT>"};if c.GitCase{repo:=filepath.Join(state,"repo");wt:=filepath.Join(state,"worktree");_=os.MkdirAll(repo,0700);gitStart(t,repo,"init","-b","main");gitStart(t,repo,"config","user.name","Oracle");gitStart(t,repo,"config","user.email","oracle@example.invalid");if e:=os.WriteFile(filepath.Join(repo,"base.txt"),[]byte("base\n"),0600);e!=nil{t.Fatal(e)};gitStart(t,repo,"add","base.txt");gitStart(t,repo,"commit","-m","base");oldHead:=gitStart(t,repo,"rev-parse","HEAD");oldTree:=gitStart(t,repo,"rev-parse","HEAD^{tree}");gitStart(t,repo,"worktree","add","--detach",wt,"HEAD");if e:=os.WriteFile(filepath.Join(wt,"applied.txt"),[]byte("already applied\n"),0600);e!=nil{t.Fatal(e)};if e:=os.WriteFile(filepath.Join(repo,"applied.txt"),[]byte("already applied\n"),0600);e!=nil{t.Fatal(e)};gitStart(t,repo,"add","applied.txt");gitStart(t,repo,"commit","-m","apply");head:=gitStart(t,repo,"rev-parse","HEAD");tree:=gitStart(t,repo,"rev-parse","HEAD^{tree}");expand["<ROOT>"]=state;expand["<TREE>"]=tree;expand["<HEAD>"]=head;expand["<OLD_TREE>"]=oldTree;expand["<OLD_HEAD>"]=oldHead;normalize[tree]="<TREE>";normalize[head]="<HEAD>";normalize[oldTree]="<OLD_TREE>";normalize[oldHead]="<OLD_HEAD>"}
	dir:=filepath.Join(state,"codex_exec_mcp","jobs");_=os.MkdirAll(dir,0700);seed:=replaceStart(c.Seed,expand).([]map[string]any);for _,s:=range seed{b,_:=json.Marshal(s);if e:=os.WriteFile(filepath.Join(dir,s["jobId"].(string)+".json"),b,0600);e!=nil{t.Fatal(e)}};if c.Thread!=nil{thread:=replaceStart(c.Thread,expand).(map[string]any);if e:=SaveRecord(filepath.Join(state,"codex_exec_mcp","threads"),thread);e!=nil{t.Fatal(e)}}
	launched:=[]map[string]any{};decisions:=[]map[string]any{};opt:=StartOptions{StateRoot:state,Validation:StartValidationOptions{Agent:"codex",FixedAgent:true,ValidateAgent:func(string)error{return nil}},RecordDecision:func(j map[string]any,d,k,s string){decisions=append(decisions,map[string]any{"jobId":j["jobId"],"decision":d,"kind":k,"sourceJobId":s})},Launch:func(id string,g int64,_ string)error{launched=append(launched,map[string]any{"jobId":id,"generation":g,"token":"TOKEN"});return nil},Await:func(id string,_ *Record)map[string]any{return jobResult(map[string]any{"jobId":id,"state":"QUEUED","acceptedAt":10,"deadlineAt":20})}}
	var got map[string]any;calls:=replaceStart(c.Calls,expand).([]map[string]any);for _,a:=range calls{var e error;got,_,e=Start(state,a,opt);if e!=nil{t.Fatal(e)}}
	files:=map[string]any{};entries,_:=os.ReadDir(dir);for _,p:=range entries{if p.IsDir()||filepath.Ext(p.Name())==".owner-lock"{files[p.Name()]=true}else{b,_:=os.ReadFile(filepath.Join(dir,p.Name()));var v any;_=json.Unmarshal(b,&v);files[p.Name()]=normalizeStart(v,normalize)}};if len(files)==0{files=nil};if !sameStart(normalizeStart(got,normalize),c.Result)||!sameStart(files,c.Files)||!sameStart(launched,c.Launches)||!sameStart(decisions,c.Decisions){t.Fatalf("start parity mismatch: %#v files=%#v decisions=%#v",got,files,decisions)}
}

func gitStart(t *testing.T,dir string,args ...string)string{t.Helper();c:=exec.Command("git",args...);c.Dir=dir;b,e:=c.CombinedOutput();if e!=nil{t.Fatalf("git %v: %v: %s",args,e,b)};return strings.TrimSpace(string(b))}
func replaceStart(v any,replacements map[string]string)any{switch x:=v.(type){case map[string]any:for k,y:=range x{x[k]=replaceStart(y,replacements)};return x;case []any:for i:=range x{x[i]=replaceStart(x[i],replacements)};return x;case []map[string]any:for i:=range x{x[i]=replaceStart(x[i],replacements).(map[string]any)};return x;case string:for from,to:=range replacements{x=strings.ReplaceAll(x,from,to)};return x};return v}

func normalizeStart(v any,r map[string]string)any{switch x:=v.(type){case map[string]any:for k,y:=range x{if k=="acceptedAt"{x[k]="TIME"}else if k=="deadlineAt"{x[k]="DEADLINE"}else if k=="signalToken"||k=="ownershipToken"{x[k]="TOKEN"}else{x[k]=normalizeStart(y,r)}};return x;case []any:for i:=range x{x[i]=normalizeStart(x[i],r)};return x;case string:for from,to:=range r{x=strings.ReplaceAll(x,from,to)};return x};return v}
func sameStart(a,b any)bool{x,_:=json.Marshal(a);y,_:=json.Marshal(b);return string(x)==string(y)}
