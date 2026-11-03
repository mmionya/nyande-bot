"""Regenerate fixtures with python-docx, python-pptx, openpyxl, reportlab and Pillow.
The Go tests read the checked-in files; Python is not a test dependency.
"""
from pathlib import Path
from io import BytesIO
from zipfile import ZipFile, ZIP_DEFLATED
from PIL import Image
from docx import Document
from docx.shared import Inches
from pptx import Presentation
from openpyxl import Workbook
from openpyxl.drawing.image import Image as ExcelImage
from reportlab.pdfgen import canvas
from reportlab.lib.utils import ImageReader

root = Path(__file__).parent
pic = BytesIO()
Image.new('RGB', (40, 30), (240, 20, 30)).save(pic, 'PNG')
image = pic.getvalue()

doc = Document()
doc.add_paragraph('Document sample — Привет, кот!')
table = doc.add_table(rows=1, cols=2)
table.cell(0, 0).text = 'Revenue'
table.cell(0, 1).text = '42'
doc.add_picture(BytesIO(image), width=Inches(1))
doc.save(root / 'sample.docx')

prs = Presentation()
for number in [1, 2]:
    slide = prs.slides.add_slide(prs.slide_layouts[6])
    slide.shapes.add_textbox(0, 0, 3000000, 1000000).text = f'Slide {number} — Привет, кот!'
    slide.shapes.add_picture(BytesIO(image), 0, 1000000)
prs.save(root / 'sample.pptx')

wb = Workbook()
ws = wb.active
ws.title = 'Budget'
ws.append(['Revenue', 42, True])
ws['D2'] = '=B1*2'
ws.add_image(ExcelImage(BytesIO(image)), 'A4')
wb.create_sheet('Notes')['A1'] = 'Привет, кот!'
wb.save(root / 'sample.xlsx')

namespaces = ' '.join(f'xmlns:{prefix}="{value}"' for prefix, value in {
 'office':'urn:oasis:names:tc:opendocument:xmlns:office:1.0',
 'text':'urn:oasis:names:tc:opendocument:xmlns:text:1.0',
 'table':'urn:oasis:names:tc:opendocument:xmlns:table:1.0',
 'draw':'urn:oasis:names:tc:opendocument:xmlns:drawing:1.0',
 'xlink':'http://www.w3.org/1999/xlink', 'math':'http://www.w3.org/1998/Math/MathML',
}.items())
for ext, subtype, body in [
 ('odt', 'text', '<text:p>Привет, кот! <text:span>Document sample</text:span></text:p>'),
 ('ods', 'spreadsheet', '<table:table table:name="Budget"><table:table-row><table:table-cell><text:p>Revenue</text:p></table:table-cell><table:table-cell office:value-type="float" office:value="42"/></table:table-row></table:table>'),
 ('odp', 'presentation', '<draw:page draw:name="Slide 1"><text:p>Привет, кот!</text:p></draw:page>'),
 ('odf', 'formula', '<math:math><math:semantics><math:annotation encoding="StarMath 5.0">a + b</math:annotation></math:semantics></math:math>'),
]:
    with ZipFile(root / f'sample.{ext}', 'w', ZIP_DEFLATED) as z:
        z.writestr('mimetype', f'application/vnd.oasis.opendocument.{subtype}')
        picture = '<draw:frame><draw:image xlink:href="Pictures/image.png"/></draw:frame>'
        z.writestr('content.xml', f'<?xml version="1.0"?><office:document-content {namespaces}><office:body><office:{subtype}>{body}{picture}</office:{subtype}></office:body></office:document-content>')
        z.writestr('Pictures/image.png', image)

for name, pages, text in [('sample.pdf', 7, True), ('scanned.pdf', 1, False)]:
    pdf = canvas.Canvas(str(root / name), pagesize=(240, 240), invariant=1)
    for page in range(pages):
        if text:
            pdf.drawString(20, 200, f'Document sample page {page + 1}')
        pdf.drawImage(ImageReader(BytesIO(image)), 20, 20, width=100, height=100)
        pdf.showPage()
    pdf.save()
