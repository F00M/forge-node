// A GraphQL document parser: operations, variables, fields with aliases and arguments, directives,
// fragments (inline and named), and every literal kind (including block strings).

export type Value =
  | { kind: 'var'; name: string }
  | { kind: 'int'; value: number }
  | { kind: 'float'; value: number }
  | { kind: 'string'; value: string }
  | { kind: 'bool'; value: boolean }
  | { kind: 'null' }
  | { kind: 'enum'; value: string }
  | { kind: 'list'; values: Value[] }
  | { kind: 'object'; fields: Array<[string, Value]> };

export interface Directive {
  name: string;
  args: Array<[string, Value]>;
}

export interface Field {
  kind: 'field';
  alias: string | null;
  name: string;
  args: Array<[string, Value]>;
  directives: Directive[];
  selections: Selection[] | null;
  line: number;
  column: number;
}

export interface InlineFragment {
  kind: 'inline';
  typeCondition: string | null;
  directives: Directive[];
  selections: Selection[];
}

export interface FragmentSpread {
  kind: 'spread';
  name: string;
  directives: Directive[];
  line: number;
  column: number;
}

export type Selection = Field | InlineFragment | FragmentSpread;

export interface VariableDefinition {
  name: string;
  type: string;
  defaultValue: Value | null;
}

export interface Operation {
  kind: 'query' | 'mutation' | 'subscription';
  name: string | null;
  variables: VariableDefinition[];
  directives: Directive[];
  selections: Selection[];
}

export interface Fragment {
  name: string;
  typeCondition: string;
  selections: Selection[];
}

export interface Document {
  operations: Operation[];
  fragments: Map<string, Fragment>;
}

export class GraphQLSyntaxError extends Error {
  readonly line: number;
  readonly column: number;
  constructor(message: string, line: number, column: number) {
    super(message);
    this.line = line;
    this.column = column;
  }
}

interface Token {
  kind: 'punct' | 'name' | 'int' | 'float' | 'string' | 'eof';
  value: string;
  line: number;
  column: number;
}

function tokenize(source: string): Token[] {
  const tokens: Token[] = [];
  let i = 0;
  let line = 1;
  let lineStart = 0;
  const n = source.length;
  while (i < n) {
    const c = source.charCodeAt(i);
    if (c === 0x0a) {
      i += 1;
      line += 1;
      lineStart = i;
      continue;
    }
    if (c === 0x0d) {
      i += 1;
      if (source.charCodeAt(i) === 0x0a) i += 1;
      line += 1;
      lineStart = i;
      continue;
    }
    if (c === 0x20 || c === 0x09 || c === 0x2c || c === 0xfeff) {
      i += 1;
      continue;
    }
    if (c === 0x23) {
      while (i < n && source.charCodeAt(i) !== 0x0a && source.charCodeAt(i) !== 0x0d) i += 1;
      continue;
    }
    const column = i - lineStart + 1;
    const ch = source[i]!;
    if ('!$&()[]{}:=@|'.includes(ch)) {
      tokens.push({ kind: 'punct', value: ch, line, column });
      i += 1;
      continue;
    }
    if (ch === '.') {
      if (source.startsWith('...', i)) {
        tokens.push({ kind: 'punct', value: '...', line, column });
        i += 3;
        continue;
      }
      throw new GraphQLSyntaxError(`Parse error on "." (error) at [${line}, ${column}]`, line, column);
    }
    if (/[_A-Za-z]/.test(ch)) {
      let j = i + 1;
      while (j < n && /[_0-9A-Za-z]/.test(source[j]!)) j += 1;
      tokens.push({ kind: 'name', value: source.slice(i, j), line, column });
      i = j;
      continue;
    }
    if (ch === '-' || (c >= 0x30 && c <= 0x39)) {
      let j = i;
      if (source[j] === '-') j += 1;
      while (j < n && /[0-9]/.test(source[j]!)) j += 1;
      let isFloat = false;
      if (source[j] === '.') {
        isFloat = true;
        j += 1;
        while (j < n && /[0-9]/.test(source[j]!)) j += 1;
      }
      if (source[j] === 'e' || source[j] === 'E') {
        isFloat = true;
        j += 1;
        if (source[j] === '+' || source[j] === '-') j += 1;
        while (j < n && /[0-9]/.test(source[j]!)) j += 1;
      }
      tokens.push({ kind: isFloat ? 'float' : 'int', value: source.slice(i, j), line, column });
      i = j;
      continue;
    }
    if (ch === '"') {
      if (source.startsWith('"""', i)) {
        let j = i + 3;
        let raw = '';
        while (j < n && !source.startsWith('"""', j)) {
          if (source.startsWith('\\"""', j)) {
            raw += '"""';
            j += 4;
            continue;
          }
          if (source[j] === '\n') {
            line += 1;
            lineStart = j + 1;
          }
          raw += source[j];
          j += 1;
        }
        if (j >= n) throw new GraphQLSyntaxError('Unterminated string', line, column);
        tokens.push({ kind: 'string', value: blockString(raw), line, column });
        i = j + 3;
        continue;
      }
      let j = i + 1;
      let value = '';
      while (j < n && source[j] !== '"') {
        const d = source[j]!;
        if (d === '\n' || d === '\r') throw new GraphQLSyntaxError('Unterminated string', line, column);
        if (d === '\\') {
          const e = source[j + 1];
          if (e === 'u') {
            const hex = source.slice(j + 2, j + 6);
            value += String.fromCharCode(Number.parseInt(hex, 16));
            j += 6;
            continue;
          }
          const map: Record<string, string> = { '"': '"', '\\': '\\', '/': '/', b: '\b', f: '\f', n: '\n', r: '\r', t: '\t' };
          value += map[e ?? ''] ?? e ?? '';
          j += 2;
          continue;
        }
        value += d;
        j += 1;
      }
      if (j >= n) throw new GraphQLSyntaxError('Unterminated string', line, column);
      tokens.push({ kind: 'string', value, line, column });
      i = j + 1;
      continue;
    }
    throw new GraphQLSyntaxError(`Parse error on "${ch}" (error) at [${line}, ${column}]`, line, column);
  }
  tokens.push({ kind: 'eof', value: '', line, column: i - lineStart + 1 });
  return tokens;
}

function blockString(raw: string): string {
  const lines = raw.split(/\r\n|\n|\r/);
  let common: number | null = null;
  for (let k = 1; k < lines.length; k += 1) {
    const text = lines[k]!;
    const indent = text.length - text.replace(/^[ \t]*/, '').length;
    if (indent < text.length && (common === null || indent < common)) common = indent;
  }
  if (common !== null) for (let k = 1; k < lines.length; k += 1) lines[k] = lines[k]!.slice(common);
  while (lines.length > 0 && lines[0]!.trim() === '') lines.shift();
  while (lines.length > 0 && lines[lines.length - 1]!.trim() === '') lines.pop();
  return lines.join('\n');
}

class Parser {
  private readonly tokens: Token[];
  private pos = 0;

  constructor(source: string) {
    this.tokens = tokenize(source);
  }

  private peek(): Token {
    return this.tokens[this.pos]!;
  }

  private next(): Token {
    const token = this.tokens[this.pos]!;
    if (token.kind !== 'eof') this.pos += 1;
    return token;
  }

  private fail(token: Token): never {
    const shown = token.kind === 'eof' ? 'end of file' : `"${token.value}"`;
    throw new GraphQLSyntaxError(`Parse error on ${shown} (${token.kind === 'eof' ? 'EOF' : token.kind.toUpperCase()}) at [${token.line}, ${token.column}]`, token.line, token.column);
  }

  private isPunct(value: string): boolean {
    const token = this.peek();
    return token.kind === 'punct' && token.value === value;
  }

  private expectPunct(value: string): Token {
    const token = this.next();
    if (token.kind !== 'punct' || token.value !== value) this.fail(token);
    return token;
  }

  private expectName(): Token {
    const token = this.next();
    if (token.kind !== 'name') this.fail(token);
    return token;
  }

  parseDocument(): Document {
    const operations: Operation[] = [];
    const fragments = new Map<string, Fragment>();
    while (this.peek().kind !== 'eof') {
      const token = this.peek();
      if (token.kind === 'punct' && token.value === '{') {
        operations.push({ kind: 'query', name: null, variables: [], directives: [], selections: this.parseSelectionSet() });
        continue;
      }
      if (token.kind === 'name' && (token.value === 'query' || token.value === 'mutation' || token.value === 'subscription')) {
        this.next();
        let name: string | null = null;
        if (this.peek().kind === 'name') name = this.next().value;
        const variables = this.isPunct('(') ? this.parseVariableDefinitions() : [];
        const directives = this.parseDirectives(false);
        operations.push({ kind: token.value as Operation['kind'], name, variables, directives, selections: this.parseSelectionSet() });
        continue;
      }
      if (token.kind === 'name' && token.value === 'fragment') {
        this.next();
        const name = this.expectName().value;
        const on = this.expectName();
        if (on.value !== 'on') this.fail(on);
        const typeCondition = this.expectName().value;
        this.parseDirectives(false);
        fragments.set(name, { name, typeCondition, selections: this.parseSelectionSet() });
        continue;
      }
      this.fail(token);
    }
    if (operations.length === 0 && fragments.size === 0) this.fail(this.peek());
    return { operations, fragments };
  }

  private parseVariableDefinitions(): VariableDefinition[] {
    this.expectPunct('(');
    const out: VariableDefinition[] = [];
    while (!this.isPunct(')')) {
      this.expectPunct('$');
      const name = this.expectName().value;
      this.expectPunct(':');
      const type = this.parseType();
      let defaultValue: Value | null = null;
      if (this.isPunct('=')) {
        this.next();
        defaultValue = this.parseValue(true);
      }
      this.parseDirectives(true);
      out.push({ name, type, defaultValue });
    }
    this.expectPunct(')');
    return out;
  }

  private parseType(): string {
    let type: string;
    if (this.isPunct('[')) {
      this.next();
      type = `[${this.parseType()}]`;
      this.expectPunct(']');
    } else {
      type = this.expectName().value;
    }
    if (this.isPunct('!')) {
      this.next();
      type += '!';
    }
    return type;
  }

  private parseDirectives(isConst: boolean): Directive[] {
    const out: Directive[] = [];
    while (this.isPunct('@')) {
      this.next();
      const name = this.expectName().value;
      const args = this.isPunct('(') ? this.parseArguments(isConst) : [];
      out.push({ name, args });
    }
    return out;
  }

  private parseArguments(isConst: boolean): Array<[string, Value]> {
    this.expectPunct('(');
    const out: Array<[string, Value]> = [];
    while (!this.isPunct(')')) {
      const name = this.expectName().value;
      this.expectPunct(':');
      out.push([name, this.parseValue(isConst)]);
    }
    this.expectPunct(')');
    return out;
  }

  private parseValue(isConst: boolean): Value {
    const token = this.next();
    if (token.kind === 'punct') {
      if (token.value === '$' && !isConst) return { kind: 'var', name: this.expectName().value };
      if (token.value === '[') {
        const values: Value[] = [];
        while (!this.isPunct(']')) values.push(this.parseValue(isConst));
        this.next();
        return { kind: 'list', values };
      }
      if (token.value === '{') {
        const fields: Array<[string, Value]> = [];
        while (!this.isPunct('}')) {
          const name = this.expectName().value;
          this.expectPunct(':');
          fields.push([name, this.parseValue(isConst)]);
        }
        this.next();
        return { kind: 'object', fields };
      }
      this.fail(token);
    }
    if (token.kind === 'int') return { kind: 'int', value: Number.parseInt(token.value, 10) };
    if (token.kind === 'float') return { kind: 'float', value: Number.parseFloat(token.value) };
    if (token.kind === 'string') return { kind: 'string', value: token.value };
    if (token.kind === 'name') {
      if (token.value === 'true') return { kind: 'bool', value: true };
      if (token.value === 'false') return { kind: 'bool', value: false };
      if (token.value === 'null') return { kind: 'null' };
      return { kind: 'enum', value: token.value };
    }
    this.fail(token);
  }

  private parseSelectionSet(): Selection[] {
    this.expectPunct('{');
    const out: Selection[] = [];
    while (!this.isPunct('}')) out.push(this.parseSelection());
    this.next();
    if (out.length === 0) this.fail(this.tokens[this.pos - 1]!);
    return out;
  }

  private parseSelection(): Selection {
    if (this.isPunct('...')) {
      const spread = this.next();
      const token = this.peek();
      if (token.kind === 'name' && token.value === 'on') {
        this.next();
        const typeCondition = this.expectName().value;
        const directives = this.parseDirectives(false);
        return { kind: 'inline', typeCondition, directives, selections: this.parseSelectionSet() };
      }
      if (token.kind === 'name') {
        this.next();
        return { kind: 'spread', name: token.value, directives: this.parseDirectives(false), line: spread.line, column: spread.column };
      }
      const directives = this.parseDirectives(false);
      return { kind: 'inline', typeCondition: null, directives, selections: this.parseSelectionSet() };
    }
    const first = this.expectName();
    let alias: string | null = null;
    let name = first.value;
    if (this.isPunct(':')) {
      this.next();
      alias = first.value;
      name = this.expectName().value;
    }
    const args = this.isPunct('(') ? this.parseArguments(false) : [];
    const directives = this.parseDirectives(false);
    const selections = this.isPunct('{') ? this.parseSelectionSet() : null;
    return { kind: 'field', alias, name, args, directives, selections, line: first.line, column: first.column };
  }
}

export function parseDocument(source: string): Document {
  return new Parser(source).parseDocument();
}
