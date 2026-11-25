// How many browsers run at once (used by playwright.config.ts), and a small
// report of the machine for the pipeline's plan summary:
//   node e2e/workers.cjs <projects>   prints one JSON line
// One browser per CPU, and 1 GB of memory each after 700 MB for the runner,
// Playwright and the app instances (measured on the test server: two browsers
// in a 1.7 GB job spent their time reclaiming). The memory is the job's own
// limit when it has one (the lab's test server gives each job a share), else
// the machine's free memory.
const fs = require('fs');
const os = require('os');
const path = require('path');

function jobMemoryLimit() {
  try {
    const own = fs.readFileSync('/proc/self/cgroup', 'utf8').trim().split(':').pop() || '';
    const max = fs.readFileSync(path.join('/sys/fs/cgroup', own, 'memory.max'), 'utf8').trim();
    if (max !== 'max') return Number(max);
  } catch {
    // no cgroup v2 here: the machine's free memory decides
  }
  return null;
}

function parallelBrowsers(projects) {
  let memory = os.freemem();
  const limit = jobMemoryLimit();
  if (limit !== null) memory = Math.min(memory, limit);
  const byMemory = Math.floor((memory - 700 * 2 ** 20) / 2 ** 30);
  return Math.max(1, Math.min(projects, os.availableParallelism(), byMemory));
}

module.exports = { parallelBrowsers, jobMemoryLimit };

if (require.main === module) {
  const projects = Number(process.argv[2] || 1);
  const mb = (n) => (n === null ? null : Math.round(n / 2 ** 20));
  console.log(JSON.stringify({
    total_mb: mb(os.totalmem()),
    free_mb: mb(os.freemem()),
    job_limit_mb: mb(jobMemoryLimit()),
    cpus: os.availableParallelism(),
    projects,
    workers: parallelBrowsers(projects),
  }));
}
