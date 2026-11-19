// Agent prompts, in a file an operator can edit without touching the agent.
//
// The defaults below ship in the repo. `GLIPH_PROMPTS=/path/prompts.json`
// overrides any section; sections it does not mention keep their defaults.
//
// THE ONE RULE THAT MAKES THIS SAFE TO EXPOSE: a bad override changes NOTHING.
// Not "changes what it can and warns about the rest" — nothing. Unparseable
// JSON, an unknown section name (which is almost always a typo for a real one),
// or a template missing a placeholder its caller will substitute: all of them
// leave the shipped defaults in place and say why on stderr.
//
// The failure this prevents is specific and nasty. A half-applied prompt file
// leaves an agent running with one section overridden and another silently
// reverted, which looks like the model behaving strangely rather than like a
// config error — and people debug the model for hours before they check the
// file. All or nothing is legible; partial is not.

import { readFileSync } from "node:fs";

/** Placeholders each section MUST still contain after an override. If a
 *  template loses one, the caller's substitution silently drops the context it
 *  was supposed to carry — the agent then answers with no channel history and
 *  nobody can see why. */
const REQUIRED = {
  system: ["{name}"],
  ask: ["{context}", "{question}"],
  summarise: [],
};

export const DEFAULTS = {
  system:
    "You are {name}, an assistant living in a text-first team chat channel. " +
    "Answer in plain prose, no markdown headers or bullet lists — the client renders " +
    "monospace text, not rich formatting. Be brief: two or three sentences unless asked " +
    "for more. You can see recent channel messages for context; do not repeat them back.",

  ask: "Recent channel messages:\n{context}\n\nQuestion: {question}",

  summarise: "Summarise what this channel has been discussing, in three sentences.",
};

let active = { ...DEFAULTS };

/** Validate an override without applying it. Returns an error string, or null
 *  when the whole thing is good. */
export function validate(obj) {
  if (obj === null || typeof obj !== "object" || Array.isArray(obj)) {
    return "prompts must be a JSON object";
  }
  for (const section of Object.keys(obj)) {
    if (!(section in DEFAULTS)) {
      return `unknown section "${section}" — expected one of ${Object.keys(DEFAULTS).join(", ")}`;
    }
    if (typeof obj[section] !== "string") {
      return `section "${section}" must be a string`;
    }
    for (const placeholder of REQUIRED[section]) {
      if (!obj[section].includes(placeholder)) {
        return `section "${section}" must still contain ${placeholder}`;
      }
    }
  }
  return null;
}

/** Apply an override. Returns null on success or the reason it was refused —
 *  and on refusal, `active` is exactly what it was before. */
export function setOverrides(obj) {
  const err = validate(obj);
  if (err) return err;
  active = { ...DEFAULTS, ...obj };
  return null;
}

/** Load from GLIPH_PROMPTS if it is set. Never throws: an agent that cannot
 *  read its prompt file still runs, on the prompts it shipped with. */
export function loadFromEnv(envVar = "GLIPH_PROMPTS") {
  const file = process.env[envVar];
  if (!file) return;
  let parsed;
  try {
    parsed = JSON.parse(readFileSync(file, "utf8"));
  } catch (err) {
    console.error(`[prompts] ignoring ${file}: ${err.message} — using defaults`);
    return;
  }
  const err = setOverrides(parsed);
  if (err) console.error(`[prompts] ignoring ${file}: ${err} — using defaults`);
  else console.log(`[prompts] loaded overrides from ${file}`);
}

/** Render a section, substituting {placeholders}. Unknown placeholders are left
 *  as written rather than blanked: a visible `{typo}` in the output is how the
 *  author finds out, and an empty string is how they don't. */
export function prompt(section, vars = {}) {
  const template = active[section];
  if (template === undefined) throw new Error(`no prompt section "${section}"`);
  return template.replace(/\{(\w+)\}/g, (whole, key) =>
    Object.prototype.hasOwnProperty.call(vars, key) ? String(vars[key]) : whole);
}

/** For tests and for a `--show-prompts` flag. */
export const current = () => ({ ...active });
export const reset = () => { active = { ...DEFAULTS }; };
