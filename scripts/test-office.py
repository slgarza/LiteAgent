#!/usr/bin/env python3
"""Create/verify synthetic Office fixtures for TestOfficeApplicationCompatibility.

Optional test dependencies: python-docx, python-pptx, openpyxl. Never pass user
files: create and verify operate only on fixed fixture names in a test directory.
"""
import datetime
from pathlib import Path
import sys

from docx import Document
from openpyxl import Workbook, load_workbook
from openpyxl.styles import Font, PatternFill
from pptx import Presentation
from pptx.util import Inches


def create(folder):
    document = Document()
    paragraph = document.add_paragraph()
    paragraph.add_run('Hola ').bold = True
    paragraph.add_run('mundo').italic = True
    document.add_table(rows=1, cols=1).cell(0, 0).text = 'Tabla intacta'
    document.sections[0].header.paragraphs[0].text = 'Encabezado intacto'
    document.save(folder / 'document.docx')

    presentation = Presentation()
    slide = presentation.slides.add_slide(presentation.slide_layouts[6])
    frame = slide.shapes.add_textbox(Inches(1), Inches(1), Inches(6), Inches(1)).text_frame
    run = frame.paragraphs[0].add_run()
    run.text = 'Hola '
    run.font.bold = True
    frame.paragraphs[0].add_run().text = 'mundo'
    table = slide.shapes.add_table(1, 1, Inches(1), Inches(3), Inches(4), Inches(1)).table
    table.cell(0, 0).text = 'Tabla intacta'
    presentation.save(folder / 'deck.pptx')

    workbook = Workbook()
    sheet = workbook.active
    sheet.title = 'Ventas'
    sheet['A1'] = 'Nombre'
    sheet['B2'] = 10
    sheet['B2'].font = Font(bold=True)
    sheet['B2'].fill = PatternFill('solid', fgColor='00FF00')
    sheet['C2'] = '=B2*2'
    sheet['D3'] = datetime.datetime(2026, 11, 15)
    sheet['D3'].number_format = 'yyyy-mm-dd'
    sheet.merge_cells('A4:B4')
    sheet['A4'] = 'Combinada intacta'
    workbook.save(folder / 'report.xlsx')


def verify(folder):
    document = Document(folder / 'document.docx')
    assert document.paragraphs[0].text == 'Hola equipo'
    assert document.paragraphs[0].runs[0].bold
    assert document.paragraphs[0].runs[1].italic
    assert document.tables[0].cell(0, 0).text == 'Tabla intacta'
    assert document.sections[0].header.paragraphs[0].text == 'Encabezado intacto'

    presentation = Presentation(folder / 'deck.pptx')
    assert presentation.slides[0].shapes[0].text == 'Hola equipo'
    assert presentation.slides[0].shapes[0].text_frame.paragraphs[0].runs[0].font.bold
    assert presentation.slides[0].shapes[1].table.cell(0, 0).text == 'Tabla intacta'

    workbook = load_workbook(folder / 'report.xlsx')
    sheet = workbook['Ventas']
    assert sheet['B2'].value == 25
    assert sheet['B2'].font.bold
    assert sheet['B2'].fill.fgColor.rgb == '0000FF00'
    assert sheet['C2'].value == '=B2*3'
    assert sheet['F5'].value == 'Nuevo 🌎'
    assert sheet['D3'].value == datetime.datetime(2026, 11, 15)
    assert sheet['D3'].number_format == 'yyyy-mm-dd'
    assert 'A4:B4' in sheet.merged_cells
    assert sheet['A4'].value == 'Combinada intacta'
    assert workbook.calculation.fullCalcOnLoad
    print('Independent Office readers verified values, formulas, styles, tables and headers.')


if __name__ == '__main__':
    mode, directory = sys.argv[1:]
    folder = Path(directory)
    if mode == 'create':
        create(folder)
    elif mode == 'verify':
        verify(folder)
    else:
        raise SystemExit('Expected create or verify')
