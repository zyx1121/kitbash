// ticker: the whole of a scheduled run. It prints one line that names the
// moment and the variable its unit declared, and exits 0; the run is over when
// this returns, and what it printed is what proc_logs answers with.
console.log(`ticked at ${new Date().toISOString()} for ${process.env.TICKER_NOTE ?? "nobody"}`);
// And one line on the other stream. A Process that dies says so on stderr, so
// proc_logs answering stdout alone is a member debugging with nothing, see
// issue #130: this line is what the end to end run reads back.
console.error("ticker: this line went to stderr");
