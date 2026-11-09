package webui

import (
	"bytes"
	"os/exec"
	"strings"
	"testing"
)

func TestMessageImagesBrowserBehavior(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	source, err := staticFS.ReadFile("static/chat.js")
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]string{
		"live": `
c.addMessage('user','Describe this image',true,-1,0,false,[{mediaType:'image/png',data:'aGVsbG8='}]);
assert.match(area.innerHTML,/class="message-images/,'sent messages must retain their attachment gallery');
assert.match(area.innerHTML,/src="data:image\/png;base64,aGVsbG8="/);
assert.match(area.innerHTML,/Describe this image/);
assert.equal(c.messages[0].images.length,1,'the live message must remember its own attachment snapshot');
`,
		"history": `
const history=[{role:'user',content:[{type:'image',media_type:'image/png',data:'aGVsbG8=',attachment_hash:'saved'},{type:'text',text:'Saved question'},{type:'image',media_type:'image/jpeg',data:'d29ybGQ='},{type:'text',text:'Second line'}]}];
const original=JSON.stringify(history);
assert.equal(c.appendHistoryEntries(history,'A'),1);
assert.equal((area.innerHTML.match(/class="message message-user"/g)||[]).length,1,'one user input must stay one gallery and bubble');
assert.equal((area.innerHTML.match(/class="message-image"/g)||[]).length,2);
assert.match(area.innerHTML,/Saved question[\s\S]*Second line/);
assert.match(area.innerHTML,/data-source-message-index="0"/);
assert.equal(JSON.stringify(history),original,'rendering must not rewrite persisted image blocks');
`,
		"image-only": `
assert.equal(c.appendHistoryEntries([{role:'user',content:[{type:'image',media_type:'image/png',data:'aGVsbG8='}]}],'A'),1);
assert.match(area.innerHTML,/class="message-images/);
assert.doesNotMatch(area.innerHTML,/class="message-bubble"/,'image-only messages must not create an empty text bubble');
assert.match(area.innerHTML,/data-history-turn="1"/);
`,
		"image-source": `
assert.equal(c.messageImageSource({media_type:'image/webp',data:'aGVsbG8='}),'data:image/webp;base64,aGVsbG8=');
assert.equal(c.messageImageSource({mediaType:'image/png',data:'aGVsbG8='}),'data:image/png;base64,aGVsbG8=');
assert.equal(c.messageImageSource({media_type:'text/html',data:'aGVsbG8='}),'');
assert.equal(c.messageImageSource({media_type:'image/png" onerror="alert(1)',data:'aGVsbG8='}),'');
assert.equal(c.messageImageSource({media_type:'image/png',data:'" onerror="alert(1)'}),'');
const markup=c.messageImagesMarkup([{media_type:'image/png',data:''}]);
assert.match(markup,/message-image-unavailable/,'unavailable image blocks must remain visibly represented');
assert.doesNotMatch(markup,/<img/);
`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			script := strings.Replace(streamingRenderBrowserHarness,
				"} else throw Error('unknown scenario '+scenario);", "} else {\n"+body+"\n}", 1)
			cmd := exec.Command(node, "-e", script, "message-images-"+name)
			cmd.Stdin = bytes.NewReader(source)
			if output, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("image messages %s: %v\n%s", name, err, output)
			}
		})
	}
}
