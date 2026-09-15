import { ReactNode } from "react";

interface Column<T> {
  header: string;
  render: (row: T) => ReactNode;
  className?: string;
}

interface TableProps<T> {
  columns: Column<T>[];
  rows: T[];
  keyFn: (row: T, index: number) => string;
  emptyMessage?: string;
}

export function Table<T>({ columns, rows, keyFn, emptyMessage = "Nothing here yet." }: TableProps<T>) {
  if (rows.length === 0) {
    return (
      <div className="rounded-md border border-dashed border-slate-800 p-6 text-center text-sm text-slate-500">
        {emptyMessage}
      </div>
    );
  }

  return (
    <div className="overflow-x-auto rounded-md border border-slate-800">
      <table className="min-w-full divide-y divide-slate-800 text-sm">
        <thead className="bg-slate-900/80">
          <tr>
            {columns.map((c) => (
              <th key={c.header} className="px-3 py-2 text-left font-medium text-slate-400">
                {c.header}
              </th>
            ))}
          </tr>
        </thead>
        <tbody className="divide-y divide-slate-800">
          {rows.map((row, index) => (
            <tr key={keyFn(row, index)} className="hover:bg-slate-900/40">
              {columns.map((c) => (
                <td key={c.header} className={`px-3 py-2 align-top text-slate-200 ${c.className ?? ""}`}>
                  {c.render(row)}
                </td>
              ))}
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}
