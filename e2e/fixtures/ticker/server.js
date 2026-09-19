// ticker: the whole of a scheduled run. It prints one line that names the
// moment and the variable its unit declared, and exits 0; the run is over when
// this returns, and what it printed is what proc_logs answers with.
console.log(`ticked at ${new Date().toISOString()} for ${process.env.TICKER_NOTE ?? "nobody"}`);
