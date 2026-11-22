// Example AutoChrome script.
// Open a page, read a text snapshot, save a screenshot and return a summary.
const page = ac.open({
  env: input.env || "example",
  url: input.url || "https://example.com",
});

const snap = page.snapshot({ max: 50 });
const summary = {
  title: page.title(),
  url: page.url(),
  nodes: snap.nodes.length,
  first: snap.nodes.slice(0, 5),
};

page.screenshot({ path: "example.png", fullPage: true });
page.close();
return summary;
